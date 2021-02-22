package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap/zapcore"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	victoriametricsv1beta1 "github.com/VictoriaMetrics/operator/api/v1beta1"
	"github.com/VictoriaMetrics/operator/internal/manager"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/envtest/printer"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var cfg *rest.Config
var k8sClient client.Client
var testEnv *envtest.Environment
var cancelManager context.CancelFunc
var stopped bool

func TestAPIs(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecsWithDefaultAndCustomReporters(t,
		"e2e Controller Suite",
		[]Reporter{printer.NewlineReporter{}})
}

var _ = BeforeSuite(func(done Done) {

	l := zap.New(zap.WriteTo(GinkgoWriter), zap.Level(zapcore.DebugLevel))
	logf.SetLogger(l)

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "config", "crd", "bases"),
			filepath.Join("..", "hack", "prom_crd"),
		},
		UseExistingCluster:       pointer.BoolPtr(true),
		AttachControlPlaneOutput: true,
		ErrorIfCRDPathMissing:    true,
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).ToNot(HaveOccurred())
	Expect(cfg).ToNot(BeNil())

	err = victoriametricsv1beta1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())
	//prometheus operator scheme for client
	err = monitoringv1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).ToNot(HaveOccurred())
	Expect(k8sClient).ToNot(BeNil())

	// operator settings
	os.Setenv("VM_ENABLEDPROMETHEUSCONVERTEROWNERREFERENCES", "true")

	ctx, cancel := context.WithCancel(context.Background())
	go func(ctx context.Context) {
		defer GinkgoRecover()
		err := manager.RunManager(ctx)
		stopped = true
		Expect(err).To(BeNil())
	}(ctx)
	cancelManager = cancel

	close(done)
}, 60)

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancelManager()
	Eventually(func() bool {
		return stopped
	}, 60, 2).Should(BeTrue())
	err := testEnv.Stop()
	Expect(err).ToNot(HaveOccurred())
})

//func mustDeleteObject(client client.Client, obj client.Object) error {
//	if !Expect(func() error {
//		err := client.Delete(context.Background(), obj)
//		if err != nil {
//			return err
//		}
//		return nil
//	}).To(BeNil()) {
//		return fmt.Errorf("unexpected not nil resut : %s", obj.GetName())
//	}
//	if !Eventually(func() string {
//		err := client.Get(context.Background(), types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, obj)
//		if errors.IsNotFound(err) {
//			return ""
//		}
//		if err == nil {
//			err = fmt.Errorf("expect object to be deleted: %s", obj.GetName())
//		}
//		return err.Error()
//	}, 30).Should(BeEmpty()) {
//		return fmt.Errorf("unexpected not nil resutl for : %s", obj.GetName())
//	}
//	return nil
//}
