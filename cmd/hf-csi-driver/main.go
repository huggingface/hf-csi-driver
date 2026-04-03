package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/huggingface/hf-buckets-csi-driver/pkg/driver"
	"github.com/huggingface/hf-buckets-csi-driver/pkg/webhook"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func main() {
	var (
		mode = flag.String("mode", "node", "Run mode: 'node' (CSI driver) or 'webhook' (sidecar injector)")

		// Node mode flags
		endpoint         = flag.String("endpoint", "unix:///var/lib/kubelet/plugins/hf.csi.huggingface.co/csi.sock", "CSI endpoint")
		nodeID           = flag.String("node-id", "", "Node ID")
		cacheDir         = flag.String("cache-dir", driver.DefaultCacheBase, "Base directory for volume caches")
		mountImage       = flag.String("mount-image", "", "Container image for mount pods (required in node mode)")
		mountPullPolicy  = flag.String("mount-pull-policy", "IfNotPresent", "Image pull policy for mount pods")
		mountPullSecrets = flag.String("mount-pull-secrets", "", "Comma-separated image pull secret names for mount pods")
		mountServiceAcct = flag.String("mount-service-account", "hf-csi-driver", "Service account for mount pods")
		mountHostNetwork = flag.Bool("mount-host-network", true, "Enable hostNetwork on mount pods")
		sidecarMode      = flag.Bool("sidecar-mode", false, "Use sidecar fd-passing instead of mount pods")
		namespace        = flag.String("namespace", "kube-system", "Namespace for mount pods")

		// Webhook mode flags
		webhookPort    = flag.Int("webhook-port", 22030, "Webhook server port")
		webhookCertDir = flag.String("webhook-cert-dir", "/etc/tls-certs", "Directory containing TLS cert and key")
		sidecarImage   = flag.String("sidecar-image", "", "Container image for the sidecar mounter (required in webhook mode)")

		showVersion = flag.Bool("version", false, "Print version and exit")
	)

	klog.InitFlags(nil)
	flag.Parse()

	if *showVersion {
		fmt.Printf("hf-csi-driver %s\n", driver.Version)
		os.Exit(0)
	}

	switch *mode {
	case "node":
		runNode(*endpoint, *nodeID, *cacheDir, *mountImage, *mountPullPolicy, *mountPullSecrets, *mountServiceAcct, *namespace, *mountHostNetwork, *sidecarMode)
	case "webhook":
		runWebhook(*webhookPort, *webhookCertDir, *sidecarImage)
	default:
		klog.Fatalf("Unknown mode %q (must be 'node' or 'webhook')", *mode)
	}
}

func runNode(endpoint, nodeID, cacheDir, mountImage, mountPullPolicy, mountPullSecrets, mountServiceAcct, namespace string, mountHostNetwork, sidecarMode bool) {
	if nodeID == "" {
		hostname, err := os.Hostname()
		if err != nil {
			klog.Fatalf("Failed to get hostname: %v", err)
		}
		nodeID = hostname
	}

	if mountImage == "" {
		klog.Fatal("--mount-image is required")
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("Failed to get in-cluster config: %v", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client: %v", err)
	}
	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create dynamic Kubernetes client: %v", err)
	}

	var pullSecrets []corev1.LocalObjectReference
	if mountPullSecrets != "" {
		for _, name := range strings.Split(mountPullSecrets, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: name})
			}
		}
	}

	mounter := driver.NewPodMounter(client, dynClient, namespace, nodeID, mountImage, corev1.PullPolicy(mountPullPolicy), pullSecrets, mountServiceAcct, cacheDir, mountHostNetwork)
	drv := driver.NewDriver(endpoint, nodeID, cacheDir, sidecarMode, mounter)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		klog.Infof("Received signal %v, shutting down", sig)
		drv.Stop()
	}()

	if err := drv.Run(); err != nil {
		if err == grpc.ErrServerStopped {
			klog.Info("gRPC server stopped")
		} else {
			klog.Fatalf("Failed to run driver: %v", err)
		}
	}
}

func runWebhook(port int, certDir, sidecarImage string) {
	if sidecarImage == "" {
		klog.Fatal("--sidecar-image is required in webhook mode")
	}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: ":22031",
		WebhookServer: ctrlwebhook.NewServer(ctrlwebhook.Options{
			Port:    port,
			CertDir: certDir,
		}),
	})
	if err != nil {
		klog.Fatalf("Failed to create manager: %v", err)
	}

	if err := mgr.AddReadyzCheck("readyz", func(_ *http.Request) error { return nil }); err != nil {
		klog.Fatalf("Failed to add readyz check: %v", err)
	}

	config := webhook.Config{SidecarImage: sidecarImage}
	decoder := admission.NewDecoder(scheme)
	injector := webhook.NewInjector(config, mgr.GetAPIReader(), decoder)

	hookServer := mgr.GetWebhookServer()
	hookServer.Register("/inject", &ctrlwebhook.Admission{Handler: injector})

	klog.Infof("Starting webhook server on port %d", port)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		klog.Fatalf("Webhook server failed: %v", err)
	}
}
