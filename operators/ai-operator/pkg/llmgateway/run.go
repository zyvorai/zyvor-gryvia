package llmgateway

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	gryviav1 "github.com/zyvorai/gryvia/operators/ai-operator/api/v1"
)

// CacheSource reads keys and routes from an informer cache.
type CacheSource struct {
	Reader       client.Reader
	KeyNamespace string
	PriceIn      float64
	PriceOut     float64
}

func (s *CacheSource) Keys(ctx context.Context) (map[string]Key, error) {
	var list corev1.SecretList
	if err := s.Reader.List(ctx, &list, client.InNamespace(s.KeyNamespace), client.MatchingLabels{LabelKey: "true"}); err != nil {
		return nil, err
	}
	return keysFrom(list.Items), nil
}

func (s *CacheSource) Routes(ctx context.Context) ([]Route, error) {
	var list gryviav1.GryviaInferenceServiceList
	if err := s.Reader.List(ctx, &list); err != nil {
		return nil, err
	}
	return routesFrom(list.Items, s.PriceIn, s.PriceOut), nil
}

// Run is the llm-gateway subcommand of the operator binary.
func Run(args []string) error {
	fs := flag.NewFlagSet("llm-gateway", flag.ContinueOnError)
	listen := fs.String("listen-address", ":8080", "Address of the OpenAI-compatible API.")
	metricsAddr := fs.String("metrics-address", ":8081", "Address of /metrics and /healthz.")
	keyNamespace := fs.String("key-namespace", "gryvia-llm-keys", "Namespace of the key Secrets (label gryvia.io/llm-key=true).")
	priceIn := fs.Float64("price-input-per-1m", 0, "Default price per million input tokens (annotation gryvia.io/llm-price-input-per-1m overrides it).")
	priceOut := fs.Float64("price-output-per-1m", 0, "Default price per million output tokens (annotation gryvia.io/llm-price-output-per-1m overrides it).")
	currency := fs.String("currency", "USD", "Currency written on token usage records.")
	flush := fs.Duration("flush-interval", 5*time.Minute, "How often token buckets are written as GryviaUsageRecords.")
	quotaEvery := fs.Duration("quota-refresh-interval", 30*time.Second, "How often GryviaQuotas are reloaded.")
	syncEvery := fs.Duration("quota-sync-interval", time.Second, "How often this replica's token counts are added to the shared daily counter and the totals read back.")
	counterNs := fs.String("counter-namespace", os.Getenv("POD_NAMESPACE"), "Namespace of the shared daily token counter ConfigMaps (default $POD_NAMESPACE).")
	maxBody := fs.Int64("max-body-bytes", defaultMaxBody, "Largest accepted request body.")
	timeout := fs.Duration("upstream-timeout", 10*time.Minute, "Longest upstream request, streaming included.")
	intentFile := fs.String("intent-policy-file", "", "Optional JSON IntentPolicy path (simple/large model routing and burst URL). See docs/dra-and-intent.md.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *counterNs == "" {
		return errors.New("set --counter-namespace or POD_NAMESPACE")
	}
	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("llm-gateway")

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gryviav1.AddToScheme(scheme))
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	// Key Secrets (gryvia.io/llm-key=true) and mirrored vector-store credentials (gryvia.io/llm-key=store).
	keyLabel, err := labels.NewRequirement(LabelKey, selection.Exists, nil)
	if err != nil {
		return err
	}
	informers, err := cache.New(cfg, cache.Options{
		Scheme: scheme,
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {
				Namespaces: map[string]cache.Config{*keyNamespace: {}},
				Label:      labels.NewSelector().Add(*keyLabel),
			},
		},
	})
	if err != nil {
		return err
	}
	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	// Register the informers before the cache starts so the first request does not wait for a lazy start.
	for _, obj := range []client.Object{&corev1.Secret{}, &gryviav1.GryviaInferenceService{}, &gryviav1.GryviaVectorIndex{}} {
		if _, err := informers.GetInformer(ctx, obj); err != nil {
			return err
		}
	}
	go func() {
		if err := informers.Start(ctx); err != nil {
			log.Error(err, "cache stopped")
			cancel()
		}
	}()
	if !informers.WaitForCacheSync(ctx) {
		return errors.New("cache did not sync")
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	pod := os.Getenv("POD_NAME")
	if pod == "" {
		pod, _ = os.Hostname()
	}
	instance := Instance(pod, time.Now())
	meter := NewMeter(reg, instance)
	meter.currency = *currency
	quotas := NewQuotas(direct, *counterNs)
	if err := quotas.Refresh(ctx); err != nil {
		log.Error(err, "loading quotas; tokensPerDay is not enforced until a refresh succeeds")
	}
	if err := quotas.Sync(ctx, time.Now()); err != nil {
		log.Error(err, "reading the shared token counter; keys under a tokensPerDay quota get 503 until a sync succeeds")
	}

	source := &CacheSource{Reader: informers, KeyNamespace: *keyNamespace, PriceIn: *priceIn, PriceOut: *priceOut}
	var intent IntentPolicy
	if *intentFile != "" {
		raw, err := os.ReadFile(*intentFile)
		if err != nil {
			return fmt.Errorf("intent-policy-file: %w", err)
		}
		intent = ParseIntentPolicy(raw)
	}
	gw := &Gateway{
		Intent: intent,
		Source:    source,
		Indexes:   source,
		Meter:     meter,
		Quotas:    quotas,
		Client:    &http.Client{Timeout: *timeout},
		MaxBody:   *maxBody,
		Activator: &KubeActivator{Client: direct},
	}
	go every(ctx, *quotaEvery, func() {
		if err := quotas.Refresh(ctx); err != nil {
			log.Error(err, "refreshing quotas")
		}
	})
	go every(ctx, *syncEvery, func() {
		if err := quotas.Sync(ctx, time.Now()); err != nil {
			log.Error(err, "syncing the shared token counter")
		}
	})
	go every(ctx, *flush, func() {
		if err := meter.Flush(ctx, direct, time.Now()); err != nil {
			log.Error(err, "writing token usage records")
		}
	})

	metrics := http.NewServeMux()
	metrics.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	metrics.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	serving := &http.Server{Addr: *listen, Handler: gw, ReadHeaderTimeout: 10 * time.Second}
	monitoring := &http.Server{Addr: *metricsAddr, Handler: metrics, ReadHeaderTimeout: 5 * time.Second}
	errs := make(chan error, 2)
	go func() { errs <- serving.ListenAndServe() }()
	go func() { errs <- monitoring.ListenAndServe() }()
	log.Info("serving", "address", *listen, "keyNamespace", *keyNamespace, "instance", instance, "counterNamespace", *counterNs)
	select {
	case <-ctx.Done():
	case err = <-errs:
		cancel()
	}
	drain, c := context.WithTimeout(context.Background(), 60*time.Second)
	defer c()
	_ = serving.Shutdown(drain)
	_ = monitoring.Shutdown(drain)
	if serr := quotas.Sync(drain, time.Now()); serr != nil {
		log.Error(serr, "final sync of the shared token counter")
	}
	if ferr := meter.Flush(drain, direct, time.Now()); ferr != nil {
		log.Error(ferr, "final flush of token usage records")
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("llm gateway: %w", err)
	}
	return nil
}

func every(ctx context.Context, d time.Duration, f func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f()
		}
	}
}
