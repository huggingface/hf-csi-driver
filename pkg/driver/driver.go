package driver

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
)

const (
	DriverName = "hf.csi.huggingface.co"
)

var Version = "dev"

type Driver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedControllerServer
	csi.UnimplementedNodeServer

	endpoint string
	nodeID   string
	srv      *grpc.Server
	mounter  Mounter
}

func NewDriver(endpoint, nodeID string) *Driver {
	if os.Getenv("HF_TOKEN") == "" {
		klog.Warning("HF_TOKEN is not set; only public repos/buckets will be accessible")
	}

	return &Driver{
		endpoint: endpoint,
		nodeID:   nodeID,
		mounter:  NewProcessMounter(),
	}
}

func (d *Driver) Run() error {
	scheme, addr, err := ParseEndpoint(d.endpoint)
	if err != nil {
		return err
	}

	if scheme == "unix" {
		if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove existing socket %s: %w", addr, err)
		}
		if err := os.MkdirAll(filepath.Dir(addr), 0750); err != nil {
			return fmt.Errorf("failed to create socket directory: %w", err)
		}
	}

	listener, err := net.Listen(scheme, addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s://%s: %w", scheme, addr, err)
	}

	if scheme == "unix" {
		if err := os.Chmod(addr, 0700); err != nil {
			return fmt.Errorf("failed to chmod socket: %w", err)
		}
	}

	d.srv = grpc.NewServer()
	csi.RegisterIdentityServer(d.srv, d)
	csi.RegisterControllerServer(d.srv, d)
	csi.RegisterNodeServer(d.srv, d)

	klog.Infof("Listening on %s://%s", scheme, addr)
	return d.srv.Serve(listener)
}

func (d *Driver) Stop() {
	if d.srv != nil {
		klog.Info("Stopping gRPC server")
		d.srv.GracefulStop()
	}
}

func ParseEndpoint(endpoint string) (string, string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", "", fmt.Errorf("could not parse endpoint %q: %w", endpoint, err)
	}

	switch u.Scheme {
	case "unix":
		addr := u.Path
		if addr == "" {
			addr = u.Host
		}
		return u.Scheme, addr, nil
	case "tcp":
		return u.Scheme, u.Host, nil
	default:
		return "", "", fmt.Errorf("unsupported endpoint scheme: %q", u.Scheme)
	}
}
