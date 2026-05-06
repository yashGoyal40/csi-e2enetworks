// csi-e2enetworks runs the E2E Networks CSI driver as a single binary.
// The same binary serves as either Controller or Node (or both)
// depending on flags + which CSI services kubelet/sidecars connect to.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"

	"github.com/yashgoyal40/csi-e2enetworks/pkg/driver"
	"github.com/yashgoyal40/csi-e2enetworks/pkg/e2e"
)

func main() {
	endpoint := flag.String("endpoint", "unix:///csi/csi.sock", "CSI gRPC socket")
	mode := flag.String("mode", "all", "controller | node | all")
	flag.Parse()

	apiKey := getEnvOr("E2E_API_KEY", "")
	auth := getEnvOr("E2E_AUTH_TOKEN", "")
	projectID := getEnvOr("E2E_PROJECT_ID", "")
	location := getEnvOr("E2E_LOCATION", "Delhi")
	if apiKey == "" || auth == "" || projectID == "" {
		die("E2E_API_KEY, E2E_AUTH_TOKEN, E2E_PROJECT_ID must be set")
	}

	api := e2e.New(apiKey, auth, projectID)
	api.Location = location

	nodeIP := os.Getenv("E2E_NODE_IP")
	nodeName := os.Getenv("E2E_NODE_NAME")
	d := driver.New(api, nodeIP, nodeName)

	listener, err := dial(*endpoint)
	if err != nil {
		die("listen %s: %v", *endpoint, err)
	}

	// Register every CSI service on every binary. The mode flag is
	// informational — kubelet and the controller sidecars each only
	// call the RPCs relevant to their role. Registering all three lets
	// the controller satisfy capability-probe RPCs (NodeGetCapabilities,
	// etc.) that some sidecars (csi-resizer, csi-snapshotter) issue at
	// startup against the controller socket.
	server := grpc.NewServer()
	csi.RegisterIdentityServer(server, d)
	csi.RegisterControllerServer(server, d)
	csi.RegisterNodeServer(server, d)
	if *mode == "node" && nodeIP == "" {
		die("E2E_NODE_IP required in node mode")
	}

	// graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		fmt.Fprintln(os.Stderr, "shutting down")
		server.GracefulStop()
		cancel()
	}()

	fmt.Fprintf(os.Stderr, "csi-e2enetworks %s listening on %s (mode=%s)\n", driver.Version, *endpoint, *mode)
	if err := server.Serve(listener); err != nil {
		die("serve: %v", err)
	}
	<-ctx.Done()
}

func dial(endpoint string) (net.Listener, error) {
	scheme, addr, err := parseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if scheme == "unix" {
		_ = os.Remove(addr) // remove stale socket
		if err := os.MkdirAll(socketDir(addr), 0750); err != nil {
			return nil, err
		}
	}
	return net.Listen(scheme, addr)
}

func parseEndpoint(ep string) (string, string, error) {
	if strings.HasPrefix(ep, "unix://") {
		return "unix", strings.TrimPrefix(ep, "unix://"), nil
	}
	if strings.HasPrefix(ep, "tcp://") {
		return "tcp", strings.TrimPrefix(ep, "tcp://"), nil
	}
	return "", "", fmt.Errorf("invalid endpoint %q (need unix:// or tcp://)", ep)
}

func socketDir(addr string) string {
	if i := strings.LastIndex(addr, "/"); i > 0 {
		return addr[:i]
	}
	return "."
}

func getEnvOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func die(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
