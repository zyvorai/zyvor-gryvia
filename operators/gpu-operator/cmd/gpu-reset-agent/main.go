// Command gpu-reset-agent runs one GPU reset agent for the node named by NODE_NAME (or --node).
// Default is dry-run; --execute actually runs nvidia-smi --gpu-reset. Driver reload and node reboot
// need --allow-driver-reload / --allow-reboot as well. See pkg/agent.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/zyvorai/gryvia/operators/gpu-operator/pkg/agent"
)

func main() {
	node := flag.String("node", os.Getenv("NODE_NAME"), "node to act on (default $NODE_NAME)")
	execute := flag.Bool("execute", false, "really run nvidia-smi --gpu-reset (default: dry-run)")
	smi := flag.String("nvidia-smi", "nvidia-smi", "nvidia-smi binary to run with --execute")
	every := flag.Duration("poll", 15*time.Second, "how often to look at the node")
	allowReload := flag.Bool("allow-driver-reload", false, "allow the driver-reload action (deletes this node's GPU Operator driver pods)")
	allowReboot := flag.Bool("allow-reboot", false, "allow the reboot action")
	rebootMethod := flag.String("reboot-method", agent.RebootKured, "kured (write --reboot-sentinel) or nsenter (systemctl reboot in the host namespace)")
	sentinel := flag.String("reboot-sentinel", "/var/run/gryvia/reboot-required", "file kured watches for")
	leaseNS := flag.String("lease-namespace", os.Getenv("POD_NAMESPACE"), "namespace of the one-node-at-a-time recovery lease (default $POD_NAMESPACE)")
	flag.Parse()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		fail(err)
	}
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		fail(err)
	}
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		fail(err)
	}
	a := &agent.Agent{Client: c, Node: *node, Execute: *execute, Runner: agent.SMIRunner{Path: *smi},
		AllowDriverReload: *allowReload, AllowReboot: *allowReboot, RebootMethod: *rebootMethod,
		Rebooter: agent.HostRebooter{Sentinel: *sentinel}, LeaseNamespace: *leaseNS}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	mode := "dry-run"
	if *execute {
		mode = "EXECUTE"
	}
	fmt.Printf("gpu-reset-agent node=%s mode=%s poll=%s driver-reload=%t reboot=%t (%s)\n", *node, mode, *every, *allowReload, *allowReboot, *rebootMethod)
	err = a.Run(ctx, *every,
		func(state string) { fmt.Printf("handled a recovery request: %s\n", state) },
		func(err error) { fmt.Fprintf(os.Stderr, "reconcile: %v\n", err) })
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "gpu-reset-agent:", err)
	os.Exit(1)
}
