package driver

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
)

const (
	DriverName = "hf.csi.huggingface.co"
)

var Version = "dev"

const DefaultCacheBase = "/var/lib/hf-csi-driver/cache"

type Driver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedControllerServer
	csi.UnimplementedNodeServer

	endpoint  string
	nodeID    string
	cacheBase string
	srv       *grpc.Server
	mounter   Mounter
	stopCh    chan struct{}
	stopOnce  sync.Once
}

func NewDriver(endpoint, nodeID, cacheBase string, mounter Mounter) *Driver {
	if cacheBase == "" {
		cacheBase = DefaultCacheBase
	}
	return &Driver{
		endpoint:  endpoint,
		nodeID:    nodeID,
		cacheBase: cacheBase,
		mounter:   mounter,
		stopCh:    make(chan struct{}),
	}
}

func (d *Driver) Run() error {
	_, addr, err := ParseEndpoint(d.endpoint)
	if err != nil {
		return err
	}

	if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove existing socket %s: %w", addr, err)
	}
	if err := os.MkdirAll(filepath.Dir(addr), 0750); err != nil {
		return fmt.Errorf("failed to create socket directory: %w", err)
	}

	if err := d.mounter.Recover(); err != nil {
		klog.Warningf("Mount recovery failed: %v", err)
	}

	d.mounter.Start(d.stopCh)

	listener, err := net.Listen("unix", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on unix://%s: %w", addr, err)
	}

	if err := os.Chmod(addr, 0700); err != nil {
		return fmt.Errorf("failed to chmod socket: %w", err)
	}

	d.srv = grpc.NewServer()
	csi.RegisterIdentityServer(d.srv, d)
	csi.RegisterControllerServer(d.srv, d)
	csi.RegisterNodeServer(d.srv, d)

	klog.Infof("Listening on unix://%s", addr)
	return d.srv.Serve(listener)
}

func (d *Driver) Stop() {
	d.stopOnce.Do(func() { close(d.stopCh) })

	if d.srv == nil {
		return
	}
	klog.Info("Stopping gRPC server")

	stopped := make(chan struct{})
	go func() {
		d.srv.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		klog.Warning("Graceful stop timed out, forcing stop")
		d.srv.Stop()
	}
}

func ParseEndpoint(endpoint string) (string, string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", "", fmt.Errorf("could not parse endpoint %q: %w", endpoint, err)
	}

	if u.Scheme != "unix" {
		return "", "", fmt.Errorf("unsupported endpoint scheme: %q (only unix is supported)", u.Scheme)
	}

	addr := u.Path
	if addr == "" {
		addr = u.Host
	}
	return u.Scheme, addr, nil
}
