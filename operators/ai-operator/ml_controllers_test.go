package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zyvorai/gryvia/operators/ai-operator/controllers"
)

func TestMLFlagDefaultsKeepTheBuiltInImages(t *testing.T) {
	var o mlOptions
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	o.bind(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if !o.enabled || o.workspaceJupyterImage != controllers.DefaultJupyterImage || o.workspaceCodeImage != controllers.DefaultVSCodeImage {
		t.Errorf("workspace defaults: %+v", o)
	}
	if o.inferenceImageVLLM != "vllm/vllm-openai:latest" || o.inferenceImageTriton != "nvcr.io/nvidia/tritonserver:24.01-py3" ||
		o.inferenceImageTensorRT != "nvcr.io/nvidia/tritonserver:24.01-trtllm-python-py3" || o.inferenceImageTorchServe != "pytorch/torchserve:latest-gpu" {
		t.Errorf("inference defaults: %+v", o)
	}
	if o.autoServeGPUCount != 1 || o.workflowAllowWebhooks || o.canaryStartupGrace != 5*time.Minute || o.inferenceHealthPath != "" {
		t.Errorf("other defaults: %+v", o)
	}
}

func TestMLFlagOverrides(t *testing.T) {
	var o mlOptions
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	o.bind(fs)
	err := fs.Parse([]string{
		"--workspace-code-image=busybox:1", "--inference-image-vllm=nginx:1", "--inference-health-path=/",
		"--autoserve-default-gpu-count=0", "--workflow-allow-webhooks=true", "--enable-ml-controllers=false",
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.enabled || o.workspaceCodeImage != "busybox:1" || o.inferenceImageVLLM != "nginx:1" || o.inferenceHealthPath != "/" ||
		o.autoServeGPUCount != 0 || !o.workflowAllowWebhooks {
		t.Errorf("overrides: %+v", o)
	}
}

func TestWorkspaceProxyFlags(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "key")
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(good, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(short, []byte("tiny"), 0o600); err != nil {
		t.Fatal(err)
	}

	if p, err := (&mlOptions{}).workspaceProxy(); p != nil || err != nil {
		t.Errorf("unset: %v %v", p, err)
	}
	p, err := (&mlOptions{workspacePublicURL: "https://g.example.com", workspaceProxyKeyFile: good, platformNamespace: "gryvia"}).workspaceProxy()
	if err != nil || p == nil || string(p.TokenKey) != "0123456789abcdef0123456789abcdef" || p.PlatformNamespace != "gryvia" {
		t.Errorf("valid: %+v %v", p, err)
	}
	for name, o := range map[string]mlOptions{
		"url only":    {workspacePublicURL: "https://g.example.com"},
		"key only":    {workspaceProxyKeyFile: good},
		"bad url":     {workspacePublicURL: "g.example.com", workspaceProxyKeyFile: good},
		"short key":   {workspacePublicURL: "https://g.example.com", workspaceProxyKeyFile: short},
		"no key file": {workspacePublicURL: "https://g.example.com", workspaceProxyKeyFile: filepath.Join(dir, "missing")},
	} {
		if _, err := o.workspaceProxy(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
