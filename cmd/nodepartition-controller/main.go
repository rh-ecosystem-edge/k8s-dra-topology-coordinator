// Package main provides the Node Partition Topology Coordinator.
// This controller watches ResourceSlices from all DRA drivers, builds a cross-driver
// topology model, computes aligned partition combinations, and publishes DeviceClasses.
// A mutating webhook expands partition ResourceClaims into multi-request claims.
// It runs as a Deployment (cluster-wide) with leader election.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	klog "k8s.io/klog/v2"

	"github.com/rh-ecosystem-edge/k8s-dra-topology-coordinator/internal/controller"
	"github.com/rh-ecosystem-edge/k8s-dra-topology-coordinator/internal/health"
	"github.com/rh-ecosystem-edge/k8s-dra-topology-coordinator/internal/metrics"
	"github.com/rh-ecosystem-edge/k8s-dra-topology-coordinator/internal/webhook"
)

const (
	defaultShutdownTimeoutSec = 30
	leaseDuration             = 15 * time.Second
	renewDeadline             = 10 * time.Second
	retryPeriod               = 2 * time.Second
)

var (
	kubecfg                 string
	driverName              string
	partitionMode           string
	shutdownTimeout         time.Duration
	leaderElectionNamespace string
	leaderElectionID        string
	webhookPort             int
	tlsCertFile             string
	tlsKeyFile              string
)

var rootCmd = &cobra.Command{
	Use:   "nodepartition-controller",
	Short: "Node Partition Topology Coordinator",
	Long: `Topology coordinator that watches ResourceSlices from all DRA drivers,
builds a cross-driver topology model, computes aligned partition combinations,
publishes DeviceClasses, and expands partition claims via a mutating webhook.

Runs as a Deployment (cluster-wide) with leader election.`,
	RunE: runController,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.Flags().StringVar(&kubecfg, "kubeconfig", "",
		"Path to kubeconfig file (optional, uses in-cluster config if not specified)")
	rootCmd.Flags().StringVar(&driverName, "driver-name", "nodepartition.dra.k8s.io",
		"Coordinator identifier for DeviceClass labels and opaque config")
	rootCmd.Flags().StringVar(&partitionMode, "partition-mode", "auto",
		`Partition strategy: "auto" (groupings if ConfigMaps exist, else fixed partitions), `+
			`"partitions" (always fixed hierarchy), or "groupings" (always admin-defined groupings)`)
	rootCmd.Flags().DurationVar(&shutdownTimeout, "shutdown-timeout",
		defaultShutdownTimeoutSec*time.Second, "Maximum time to wait for graceful shutdown")
	rootCmd.Flags().StringVar(&leaderElectionNamespace, "leader-election-namespace", "kube-system",
		"Namespace for leader election lease")
	rootCmd.Flags().StringVar(&leaderElectionID, "leader-election-id", "nodepartition-controller",
		"Leader election lease name")
	rootCmd.Flags().IntVar(&webhookPort, "webhook-port", 9443,
		"Port for the mutating webhook HTTPS server")
	rootCmd.Flags().StringVar(&tlsCertFile, "tls-cert", "/etc/webhook/tls/tls.crt",
		"Path to TLS certificate for webhook server")
	rootCmd.Flags().StringVar(&tlsKeyFile, "tls-key", "/etc/webhook/tls/tls.key",
		"Path to TLS private key for webhook server")
}

func runController(_ *cobra.Command, _ []string) error {
	k8sConfig, err := clientcmd.BuildConfigFromFlags("", kubecfg)
	if err != nil {
		return fmt.Errorf("failed to load kubeconfig: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	klog.Info("Starting Node Partition Topology Coordinator")

	if !controller.ValidPartitionMode(partitionMode) {
		return fmt.Errorf("invalid --partition-mode %q: must be auto, partitions, or groupings", partitionMode)
	}

	// Start health checker regardless of leader status
	const healthCheckPort = 8081
	healthChecker := health.NewChecker(healthCheckPort, "1.0.0")
	healthChecker.AddCheck("basic", health.BasicHealthCheck())
	healthChecker.Handle("/metrics", metrics.Handler())

	if err := healthChecker.Start(ctx); err != nil {
		return fmt.Errorf("failed to start health checker: %v", err)
	}
	klog.Infof("Health check server started on port %d", healthCheckPort)

	ctrl := controller.NewController(clientset, driverName, controller.PartitionMode(partitionMode))
	if err := ctrl.StartCache(ctx); err != nil {
		return fmt.Errorf("failed to start controller cache: %v", err)
	}
	go func() {
		if err := ctrl.RunWorker(ctx); err != nil && ctx.Err() == nil {
			klog.Errorf("Controller worker failed: %v", err)
		}
	}()

	// Start webhook server after this replica has a synchronized topology model.
	webhookServer, err := startWebhookServer(ctx, clientset, driverName, ctrl.Model())
	if err != nil {
		return fmt.Errorf("failed to start webhook server: %v", err)
	}

	// Determine identity for leader election (prefer POD_NAME, fall back to hostname)
	identity := os.Getenv("POD_NAME")
	if identity == "" {
		identity, err = os.Hostname()
		if err != nil {
			return fmt.Errorf("failed to determine leader election identity: %v", err)
		}
	}

	// Set up leader election
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      leaderElectionID,
			Namespace: leaderElectionNamespace,
		},
		Client: clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: identity,
		},
	}

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   leaseDuration,
		RenewDeadline:   renewDeadline,
		RetryPeriod:     retryPeriod,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				klog.Info("Acquired leader election, enabling DeviceClass publication")
				ctrl.SetPublishing(true)
			},
			OnStoppedLeading: func() {
				klog.Info("Lost leader election")
				ctrl.SetPublishing(false)
			},
			OnNewLeader: func(newLeader string) {
				if newLeader != identity {
					klog.Infof("New leader elected: %s", newLeader)
				}
			},
		},
	})

	klog.Info("Received shutdown signal, initiating graceful shutdown")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()

	if err := webhookServer.Shutdown(shutdownCtx); err != nil {
		klog.ErrorS(err, "Error stopping webhook server")
	}

	if err := healthChecker.Stop(shutdownCtx); err != nil {
		klog.ErrorS(err, "Error stopping health checker")
	}

	klog.Info("Node Partition Topology Coordinator shutdown completed")
	return nil
}

func startWebhookServer(_ context.Context, clientset kubernetes.Interface, configuredDriverName string, model *controller.TopologyModel) (*http.Server, error) { //nolint:unparam // error return reserved for TLS validation
	expander := webhook.NewClaimExpanderWithDriver(clientset, configuredDriverName, model)

	mux := http.NewServeMux()
	mux.Handle("/mutate", expander.Handler())

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", webhookPort),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Load TLS certificate
	cert, err := tls.LoadX509KeyPair(tlsCertFile, tlsKeyFile)
	if err != nil {
		klog.Warningf("Failed to load TLS certificate from %s/%s: %v (webhook will not start)", tlsCertFile, tlsKeyFile, err)
		klog.Warning("Webhook server disabled — partition claims will not be expanded")
		return server, nil
	}

	server.TLSConfig = &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	go func() {
		klog.Infof("Starting webhook server on port %d", webhookPort)
		if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			klog.Errorf("Webhook server error: %v", err)
		}
	}()

	return server, nil
}

func main() {
	log.SetFlags(0)
	Execute()
}
