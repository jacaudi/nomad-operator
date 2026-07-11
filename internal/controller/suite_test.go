package controller

import (
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

var (
	testEnv *envtest.Environment
	cfg     *rest.Config
	k8s     client.Client
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join("..", "..", "config", "crd", "gateway-api"), // vendored experimental CRDs (Step 4)
		},
		ErrorIfCRDPathMissing: true,
	}
	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	Expect(nomadv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(gwapiv1.Install(scheme.Scheme)).To(Succeed())
	Expect(gwapiv1a2.Install(scheme.Scheme)).To(Succeed())

	k8s, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8s).NotTo(BeNil())
})

var _ = AfterSuite(func() {
	Expect(testEnv.Stop()).To(Succeed())
})
