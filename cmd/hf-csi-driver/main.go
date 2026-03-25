package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/huggingface/hf-buckets-csi-driver/pkg/driver"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

func main() {
	var (
		endpoint    = flag.String("endpoint", "unix:///var/lib/kubelet/plugins/hf.csi.huggingface.co/csi.sock", "CSI endpoint")
		nodeID      = flag.String("node-id", "", "Node ID")
		cacheDir    = flag.String("cache-dir", driver.DefaultCacheBase, "Base directory for volume caches")
		mountMode   = flag.String("mount-mode", "process", "Mount mode: 'process' (child process) or 'pod' (dedicated mount pod)")
		mountImage      = flag.String("mount-image", "", "Container image for mount pods (required when mount-mode=pod)")
		mountPullPolicy  = flag.String("mount-pull-policy", "IfNotPresent", "Image pull policy for mount pods")
		mountPullSecrets = flag.String("mount-pull-secrets", "", "Comma-separated image pull secret names for mount pods")
		namespace        = flag.String("namespace", "kube-system", "Namespace for mount pods")
		showVersion = flag.Bool("version", false, "Print version and exit")
	)

	klog.InitFlags(nil)
	flag.Parse()

	if *showVersion {
		fmt.Printf("hf-csi-driver %s\n", driver.Version)
		os.Exit(0)
	}

	if *nodeID == "" {
		hostname, err := os.Hostname()
		if err != nil {
			klog.Fatalf("Failed to get hostname: %v", err)
		}
		*nodeID = hostname
	}

	var mounter driver.Mounter
	switch *mountMode {
	case "process":
		mounter = driver.NewProcessMounter()
	case "pod":
		if *mountImage == "" {
			klog.Fatal("--mount-image is required when --mount-mode=pod")
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
		if *mountPullSecrets != "" {
			for _, name := range strings.Split(*mountPullSecrets, ",") {
				name = strings.TrimSpace(name)
				if name != "" {
					pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: name})
				}
			}
		}
		mounter = driver.NewPodMounter(client, dynClient, *namespace, *nodeID, *mountImage, corev1.PullPolicy(*mountPullPolicy), pullSecrets, *cacheDir)
	default:
		klog.Fatalf("Unknown mount mode %q (must be 'process' or 'pod')", *mountMode)
	}

	drv := driver.NewDriver(*endpoint, *nodeID, *cacheDir, mounter)

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
