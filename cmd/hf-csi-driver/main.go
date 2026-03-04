package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/huggingface/hf-buckets-csi-driver/pkg/driver"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
)

func main() {
	var (
		endpoint    = flag.String("endpoint", "unix:///var/lib/kubelet/plugins/hf.csi.huggingface.co/csi.sock", "CSI endpoint")
		nodeID      = flag.String("node-id", "", "Node ID")
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

	drv := driver.NewDriver(*endpoint, *nodeID)

	// Signal handler.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		klog.Infof("Received signal %v, shutting down", sig)
		drv.Stop()
	}()

	if err := drv.Run(); err != nil {
		// GracefulStop causes Serve to return grpc.ErrServerStopped,
		// which is a normal shutdown, not a fatal error.
		if err == grpc.ErrServerStopped {
			klog.Info("gRPC server stopped")
		} else {
			klog.Fatalf("Failed to run driver: %v", err)
		}
	}
}
