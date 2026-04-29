package driver

import (
	"os"

	corev1 "k8s.io/api/core/v1"
)

var proxyEnvNames = []string{
	"HTTP_PROXY",
	"HTTPS_PROXY",
	"NO_PROXY",
	"ALL_PROXY",
	"http_proxy",
	"https_proxy",
	"no_proxy",
	"all_proxy",
}

// ProxyEnvFromEnvironment returns proxy-related environment variables from the
// current process so child mount daemons use the same network path.
func ProxyEnvFromEnvironment() []corev1.EnvVar {
	vars := make([]corev1.EnvVar, 0, len(proxyEnvNames))
	for _, name := range proxyEnvNames {
		if value := os.Getenv(name); value != "" {
			vars = append(vars, corev1.EnvVar{Name: name, Value: value})
		}
	}
	return vars
}
