package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	monitoring "github.com/coreos/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/go-logr/logr"
	injectorv1alpha1 "github.com/oam-dev/trait-injector/api/v1alpha1"
	injectorcontroller "github.com/oam-dev/trait-injector/controllers"
	"github.com/oam-dev/trait-injector/pkg/injector"
	"github.com/oam-dev/trait-injector/pkg/plugin"
	certmanager "github.com/wonderflow/cert-manager-api/pkg/apis/certmanager/v1"
	kedav1alpha1 "github.com/wonderflow/keda-api/api/v1alpha1"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
	crdv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	oamcore "github.com/oam-dev/kubevela/apis/core.oam.dev"
	velacore "github.com/oam-dev/kubevela/apis/standard.oam.dev/v1alpha1"
	velacontroller "github.com/oam-dev/kubevela/pkg/controller"
	oamcontroller "github.com/oam-dev/kubevela/pkg/controller/core.oam.dev"
	oamv1alpha2 "github.com/oam-dev/kubevela/pkg/controller/core.oam.dev/v1alpha2"
	"github.com/oam-dev/kubevela/pkg/controller/dependency"
	"github.com/oam-dev/kubevela/pkg/controller/utils"
	velawebhook "github.com/oam-dev/kubevela/pkg/webhook"
	oamwebhook "github.com/oam-dev/kubevela/pkg/webhook/core.oam.dev/v1alpha2"
	"github.com/oam-dev/kubevela/version"
)

const (
	kubevelaName = "kubevela"
)

var (
	setupLog           = ctrl.Log.WithName(kubevelaName)
	scheme             = runtime.NewScheme()
	waitSecretTimeout  = 90 * time.Second
	waitSecretInterval = 2 * time.Second
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = crdv1.AddToScheme(scheme)
	_ = oamcore.AddToScheme(scheme)
	_ = monitoring.AddToScheme(scheme)
	_ = velacore.AddToScheme(scheme)
	_ = injectorv1alpha1.AddToScheme(scheme)
	_ = certmanager.AddToScheme(scheme)
	_ = kedav1alpha1.AddToScheme(scheme)
	// +kubebuilder:scaffold:scheme
}

func main() {

	setupLog.Info(fmt.Sprintf("KubeVela Version: %s, GIT Revision: %s.", version.VelaVersion, version.GitRevision))

	var metricsAddr, logFilePath, leaderElectionNamespace string
	var enableLeaderElection, logCompress bool
	var logRetainDate int
	var certDir string
	var webhookPort int
	var useWebhook, useTraitInjector bool
	var controllerArgs oamcontroller.Args
	var healthAddr string
	var disableCaps string

	flag.BoolVar(&useWebhook, "use-webhook", false, "Enable Admission Webhook")
	flag.BoolVar(&useTraitInjector, "use-trait-injector", false, "Enable TraitInjector")
	flag.StringVar(&certDir, "webhook-cert-dir", "/k8s-webhook-server/serving-certs", "Admission webhook cert/key dir.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "admission webhook listen address")
	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "The address the metric endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "enable-leader-election", false,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&leaderElectionNamespace, "leader-election-namespace", "",
		"Determines the namespace in which the leader election configmap will be created.")
	flag.StringVar(&logFilePath, "log-file-path", "", "The file to write logs to.")
	flag.IntVar(&logRetainDate, "log-retain-date", 7, "The number of days of logs history to retain.")
	flag.BoolVar(&logCompress, "log-compress", true, "Enable compression on the rotated logs.")
	flag.IntVar(&controllerArgs.RevisionLimit, "revision-limit", 50,
		"RevisionLimit is the maximum number of revisions that will be maintained. The default value is 50.")
	flag.StringVar(&healthAddr, "health-addr", ":9440", "The address the health endpoint binds to.")
	flag.BoolVar(&controllerArgs.ApplyOnceOnly, "apply-once-only", false,
		"For the purpose of some production environment that workload or trait should not be affected if no spec change")
	flag.StringVar(&disableCaps, "disable-caps", "", "To be disabled builtin capability list.")
	flag.Parse()

	// setup logging
	var w io.Writer
	if len(logFilePath) > 0 {
		w = zapcore.AddSync(&lumberjack.Logger{
			Filename: logFilePath,
			MaxAge:   logRetainDate, // days
			Compress: logCompress,
		})
	} else {
		w = os.Stdout
	}

	ctrl.SetLogger(zap.New(func(o *zap.Options) {
		o.Development = true
		o.DestWritter = w
	}))

	// install dependency charts first
	k8sClient, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create a kubernetes client")
		os.Exit(1)
	}
	go dependency.Install(k8sClient)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		MetricsBindAddress:      metricsAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionNamespace: leaderElectionNamespace,
		LeaderElectionID:        kubevelaName,
		Port:                    webhookPort,
		CertDir:                 certDir,
		HealthProbeBindAddress:  healthAddr,
	})
	if err != nil {
		setupLog.Error(err, "unable to create a controller manager")
		os.Exit(1)
	}

	if err := registerHealthChecks(mgr); err != nil {
		setupLog.Error(err, "unable to register ready/health checks")
		os.Exit(1)
	}

	if err := utils.CheckDisabledCapabilities(disableCaps); err != nil {
		setupLog.Error(err, "unable to get enabled capabilities")
		os.Exit(1)
	}

	if useWebhook {
		setupLog.Info("vela webhook enabled, will serving at :" + strconv.Itoa(webhookPort))
		if err = oamwebhook.Add(mgr); err != nil {
			setupLog.Error(err, "unable to setup oam runtime webhook")
			os.Exit(1)
		}
		velawebhook.Register(mgr, disableCaps)
		if err := waitWebhookSecretVolume(certDir, waitSecretTimeout, waitSecretInterval); err != nil {
			setupLog.Error(err, "unable to get webhook secret")
			os.Exit(1)
		}
	}

	if err = oamv1alpha2.Setup(mgr, controllerArgs, logging.NewLogrLogger(setupLog)); err != nil {
		setupLog.Error(err, "unable to setup the oam core controller")
		os.Exit(1)
	}

	if err = velacontroller.Setup(mgr, disableCaps); err != nil {
		setupLog.Error(err, "unable to setup the vela core controller")
		os.Exit(1)
	}

	if useTraitInjector {
		// register all service injectors
		plugin.RegisterTargetInjectors(injector.Defaults()...)

		tiWebhook := &injectorcontroller.ServiceBindingReconciler{
			Client:   mgr.GetClient(),
			Log:      ctrl.Log.WithName("controllers").WithName("ServiceBinding"),
			Scheme:   mgr.GetScheme(),
			Recorder: mgr.GetEventRecorderFor("servicebinding"),
		}
		if err = (tiWebhook).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "ServiceBinding")
			os.Exit(1)
		}
		// this has hard coded requirement "./ssl/service-injector.pem", "./ssl/service-injector.key"
		go tiWebhook.ServeAdmission()
	}

	setupLog.Info("starting the vela controller manager")

	if controllerArgs.ApplyOnceOnly {
		setupLog.Info("applyOnceOnly is enabled that means workload or trait only apply once if no spec change even they are changed by others")
	}
	if err := mgr.Start(makeSignalHandler(setupLog, k8sClient)); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
	setupLog.Info("program safely stops...")
}

// registerHealthChecks is used to create readiness&liveness probes
func registerHealthChecks(mgr ctrl.Manager) error {
	setupLog.Info("creating readiness/health check")
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return err
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	return nil
}

// waitWebhookSecretVolume waits for webhook secret ready to avoid mgr running crash
func waitWebhookSecretVolume(certDir string, timeout, interval time.Duration) error {
	start := time.Now()
	for {
		time.Sleep(interval)
		if time.Since(start) > timeout {
			return fmt.Errorf("getting webhook secret timeout after %s", timeout.String())
		}
		setupLog.Info(fmt.Sprintf("waiting webhook secret, time consumed: %d/%d seconds ...",
			int64(time.Since(start).Seconds()), int64(timeout.Seconds())))
		if _, err := os.Stat(certDir); !os.IsNotExist(err) {
			ready := func() bool {
				f, err := os.Open(filepath.Clean(certDir))
				if err != nil {
					return false
				}
				// nolint
				defer f.Close()
				// check if dir is empty
				if _, err := f.Readdir(1); errors.Is(err, io.EOF) {
					return false
				}
				// check if secret files are empty
				err = filepath.Walk(certDir, func(path string, info os.FileInfo, err error) error {
					// even Cert dir is created, cert files are still empty for a while
					if info.Size() == 0 {
						return errors.New("secret is not ready")
					}
					return nil
				})
				if err == nil {
					setupLog.Info(fmt.Sprintf("webhook secret is ready (time consumed: %d seconds)",
						int64(time.Since(start).Seconds())))
					return true
				}
				return false
			}()
			if ready {
				return nil
			}
		}
	}
}

//nolint:unparam
func makeSignalHandler(log logr.Logger, kubecli client.Client) (stopCh <-chan struct{}) {
	stop := make(chan struct{})
	c := make(chan os.Signal, 2)

	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-c
		// Do not uninstall when vela-core terminating.
		// When running on K8s, old pod will terminate after new pod running, it will cause charts uninstalled.
		// https://github.com/oam-dev/kubevela/issues/499
		// dependency.Uninstall(kubecli)
		close(stop)

		// second signal. Exit directly.
		<-c
		os.Exit(1)
	}()

	return stop
}
