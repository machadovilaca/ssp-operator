package tests

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	ginkgo_reporters "github.com/onsi/ginkgo/v2/reporters"
	. "github.com/onsi/gomega"
	osconfv1 "github.com/openshift/api/config/v1"
	openshiftroutev1 "github.com/openshift/api/route/v1"
	secv1 "github.com/openshift/api/security/v1"
	templatev1 "github.com/openshift/api/template/v1"
	promv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	apiregv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"

	"k8s.io/utils/pointer"
	kubevirtv1 "kubevirt.io/api/core/v1"
	instancetypev1alpha2 "kubevirt.io/api/instancetype/v1alpha2"
	cdiv1beta1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	lifecycleapi "kubevirt.io/controller-lifecycle-operator-sdk/api"
	qe_reporters "kubevirt.io/qe-tools/pkg/ginkgo-reporters"
	"kubevirt.io/ssp-operator/tests/env"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	sspv1beta1 "kubevirt.io/ssp-operator/api/v1beta1"
	"kubevirt.io/ssp-operator/internal/common"
)

var (
	topologyMode           = osconfv1.HighlyAvailableTopologyMode
	testScheme             *runtime.Scheme
	sspDeploymentName      = "ssp-operator"
	sspDeploymentNamespace = "kubevirt"
	sspWebhookServiceName  = "ssp-webhook-service"
)

type TestSuiteStrategy interface {
	Init()
	Cleanup()

	GetName() string
	GetNamespace() string
	GetTemplatesNamespace() string
	GetValidatorReplicas() int
	GetSSPDeploymentName() string
	GetSSPDeploymentNameSpace() string
	GetSSPWebhookServiceName() string

	GetVersionLabel() string
	GetPartOfLabel() string

	RevertToOriginalSspCr()
	SkipSspUpdateTestsIfNeeded()
	SkipUnlessHighlyAvailableTopologyMode()
	SkipUnlessSingleReplicaTopologyMode()
	SkipIfUpgradeLane()
}

type newSspStrategy struct {
	ssp *sspv1beta1.SSP
}

var _ TestSuiteStrategy = &newSspStrategy{}

func (s *newSspStrategy) Init() {
	validateDeploymentExists()

	Eventually(func() error {
		namespaceObj := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: s.GetNamespace(),
				Labels: map[string]string{
					"openshift.io/cluster-monitoring": "true",
				},
			}}
		return apiClient.Create(ctx, namespaceObj)
	}, env.Timeout(), time.Second).ShouldNot(HaveOccurred())

	Eventually(func() error {
		namespaceObj := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.GetTemplatesNamespace()}}
		return apiClient.Create(ctx, namespaceObj)
	}, env.Timeout(), time.Second).ShouldNot(HaveOccurred())

	newSsp := &sspv1beta1.SSP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.GetName(),
			Namespace: s.GetNamespace(),
			Labels: map[string]string{
				common.AppKubernetesNameLabel:      "ssp-cr",
				common.AppKubernetesManagedByLabel: "ssp-test-strategy",
				common.AppKubernetesPartOfLabel:    "hyperconverged-cluster",
				common.AppKubernetesVersionLabel:   "v0.0.0-test",
				common.AppKubernetesComponentLabel: common.AppComponentSchedule.String(),
			},
		},
		Spec: sspv1beta1.SSPSpec{
			TemplateValidator: sspv1beta1.TemplateValidator{
				Replicas: pointer.Int32Ptr(int32(s.GetValidatorReplicas())),
			},
			CommonTemplates: sspv1beta1.CommonTemplates{
				Namespace: s.GetTemplatesNamespace(),
			},
		},
	}

	Eventually(func() error {
		return apiClient.Create(ctx, newSsp)
	}, env.Timeout(), time.Second).ShouldNot(HaveOccurred())
	s.ssp = newSsp
}

func (s *newSspStrategy) Cleanup() {
	if env.ShouldSkipCleanupAfterTests() {
		return
	}

	if s.ssp != nil {
		err := apiClient.Delete(ctx, s.ssp)
		expectSuccessOrNotFound(err)
		waitForDeletion(client.ObjectKey{
			Name:      s.GetName(),
			Namespace: s.GetNamespace(),
		}, &sspv1beta1.SSP{})
	}

	err1 := apiClient.Delete(ctx, &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.GetNamespace()}})
	err2 := apiClient.Delete(ctx, &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.GetTemplatesNamespace()}})
	expectSuccessOrNotFound(err1)
	expectSuccessOrNotFound(err2)

	waitForDeletion(client.ObjectKey{Name: s.GetNamespace()}, &v1.Namespace{})
	waitForDeletion(client.ObjectKey{Name: s.GetTemplatesNamespace()}, &v1.Namespace{})
}

func (s *newSspStrategy) GetName() string {
	return "test-ssp"
}

func (s *newSspStrategy) GetNamespace() string {
	const testNamespace = "ssp-operator-functests"
	return testNamespace
}

func (s *newSspStrategy) GetTemplatesNamespace() string {
	const commonTemplatesTestNS = "ssp-operator-functests-templates"
	return commonTemplatesTestNS
}

func (s *newSspStrategy) GetValidatorReplicas() int {
	const templateValidatorReplicas = 2
	return templateValidatorReplicas
}

func (s *newSspStrategy) GetVersionLabel() string {
	return s.ssp.Labels[common.AppKubernetesVersionLabel]
}
func (s *newSspStrategy) GetPartOfLabel() string {
	return s.ssp.Labels[common.AppKubernetesPartOfLabel]
}

func (s *newSspStrategy) GetSSPDeploymentName() string {
	return sspDeploymentName
}

func (s *newSspStrategy) GetSSPDeploymentNameSpace() string {
	return sspDeploymentNamespace
}

func (s *newSspStrategy) GetSSPWebhookServiceName() string {
	return sspWebhookServiceName
}

func (s *newSspStrategy) RevertToOriginalSspCr() {
	waitForSspDeletionIfNeeded(s.ssp)
	createOrUpdateSsp(s.ssp)
}

func (s *newSspStrategy) SkipSspUpdateTestsIfNeeded() {
	// Do not skip SSP update tests in this strategy
}

func (s *newSspStrategy) SkipUnlessSingleReplicaTopologyMode() {
	if topologyMode != osconfv1.SingleReplicaTopologyMode {
		Skip("Tests that are specific for SingleReplicaTopologyMode are disabled", 1)
	}
}

func (s *newSspStrategy) SkipUnlessHighlyAvailableTopologyMode() {
	if topologyMode != osconfv1.HighlyAvailableTopologyMode {
		Skip("Tests that are specific for HighlyAvailableTopologyMode are disabled", 1)
	}
}

func (s *newSspStrategy) SkipIfUpgradeLane() {
	skipIfUpgradeLane()
}

func skipIfUpgradeLane() {
	if env.IsUpgradeLane() {
		Skip("Skipping in Upgrade Lane", 1)
	}
}

type existingSspStrategy struct {
	Name      string
	Namespace string

	ssp *sspv1beta1.SSP
}

var _ TestSuiteStrategy = &existingSspStrategy{}

func (s *existingSspStrategy) Init() {
	existingSsp := &sspv1beta1.SSP{}
	err := apiClient.Get(ctx, client.ObjectKey{Name: s.Name, Namespace: s.Namespace}, existingSsp)
	Expect(err).ToNot(HaveOccurred())

	templatesNamespace := existingSsp.Spec.CommonTemplates.Namespace
	Expect(apiClient.Get(ctx, client.ObjectKey{Name: templatesNamespace}, &v1.Namespace{}))

	validateDeploymentExists()

	s.ssp = existingSsp

	if s.sspModificationDisabled() {
		return
	}

	// Try to modify the SSP and check if it is not reverted by another operator
	defer s.RevertToOriginalSspCr()

	newReplicasCount := *existingSsp.Spec.TemplateValidator.Replicas + 1
	updateSsp(func(foundSsp *sspv1beta1.SSP) {
		foundSsp.Spec.TemplateValidator.Replicas = &newReplicasCount
	})

	Consistently(func() int32 {
		return *getSsp().Spec.TemplateValidator.Replicas
	}, 20*time.Second, time.Second).Should(Equal(newReplicasCount),
		"The SSP CR was modified outside of the test. "+
			"If the CR is managed by a controller, consider disabling modification tests by setting "+
			"SKIP_UPDATE_SSP_TESTS=true")
}

func (s *existingSspStrategy) Cleanup() {
	if s.ssp != nil {
		s.RevertToOriginalSspCr()
	}
}

func (s *existingSspStrategy) GetName() string {
	return s.Name
}

func (s *existingSspStrategy) GetNamespace() string {
	return s.Namespace
}

func (s *existingSspStrategy) GetTemplatesNamespace() string {
	if s.ssp == nil {
		panic("Strategy is not initialized")
	}
	return s.ssp.Spec.CommonTemplates.Namespace
}

func (s *existingSspStrategy) GetValidatorReplicas() int {
	if s.ssp == nil {
		panic("Strategy is not initialized")
	}
	return int(*s.ssp.Spec.TemplateValidator.Replicas)
}

func (s *existingSspStrategy) GetVersionLabel() string {
	return s.ssp.Labels[common.AppKubernetesVersionLabel]
}
func (s *existingSspStrategy) GetPartOfLabel() string {
	return s.ssp.Labels[common.AppKubernetesPartOfLabel]
}

func (s *existingSspStrategy) GetSSPDeploymentName() string {
	return sspDeploymentName
}

func (s *existingSspStrategy) GetSSPDeploymentNameSpace() string {
	return sspDeploymentNamespace
}

func (s *existingSspStrategy) GetSSPWebhookServiceName() string {
	return sspWebhookServiceName
}

func (s *existingSspStrategy) RevertToOriginalSspCr() {
	waitForSspDeletionIfNeeded(s.ssp)
	createOrUpdateSsp(s.ssp)
}

func (s *existingSspStrategy) SkipSspUpdateTestsIfNeeded() {
	if s.sspModificationDisabled() {
		Skip("Tests that update SSP CR are disabled", 1)
	}
}

func (s *existingSspStrategy) sspModificationDisabled() bool {
	return env.SkipUpdateSspTests()
}

func (s *existingSspStrategy) SkipUnlessSingleReplicaTopologyMode() {
	if topologyMode != osconfv1.SingleReplicaTopologyMode {
		Skip("Tests that are specific for HighlyAvailableTopologyMode are disabled", 1)
	}
}

func (s *existingSspStrategy) SkipUnlessHighlyAvailableTopologyMode() {
	if topologyMode != osconfv1.HighlyAvailableTopologyMode {
		Skip("Tests that are specific for SingleReplicaTopologyMode are disabled", 1)
	}
}

func (s *existingSspStrategy) SkipIfUpgradeLane() {
	skipIfUpgradeLane()
}

var (
	apiClient          client.Client
	coreClient         *kubernetes.Clientset
	ctx                context.Context
	strategy           TestSuiteStrategy
	sspListerWatcher   cache.ListerWatcher
	portForwarder      PortForwarder
	deploymentTimedOut bool
)

var _ = BeforeSuite(func() {
	existingCrName := env.ExistingCrName()
	if existingCrName == "" {
		strategy = &newSspStrategy{}
	} else {
		existingCrNamespace := env.ExistingCrNamespace()
		Expect(existingCrNamespace).ToNot(BeEmpty(), "Existing CR Namespace needs to be defined")
		strategy = &existingSspStrategy{Name: existingCrName, Namespace: existingCrNamespace}
	}

	fmt.Println(fmt.Sprintf("timeout set to %d minutes", env.Timeout()))
	fmt.Println(fmt.Sprintf("short timeout set to %d minutes", env.ShortTimeout()))

	if envTopologyMode, set := env.TopologyMode(); set {
		topologyMode = envTopologyMode
		fmt.Println(fmt.Sprintf("TopologyMode set to %s", envTopologyMode))
	}

	if envSspDeploymentName := env.SspDeploymentName(); envSspDeploymentName != "" {
		sspDeploymentName = envSspDeploymentName
		fmt.Println(fmt.Sprintf("SspDeploymentName set to %s", envSspDeploymentName))
	}

	if envSspDeploymentNamespace := env.SspDeploymentNamespace(); envSspDeploymentNamespace != "" {
		sspDeploymentNamespace = envSspDeploymentNamespace
		fmt.Println(fmt.Sprintf("SspDeploymentNamespace set to %s", envSspDeploymentNamespace))
	}

	if envSspWebhookServiceName := env.SspWebhookServiceName(); envSspWebhookServiceName != "" {
		sspWebhookServiceName = envSspWebhookServiceName
		fmt.Println(fmt.Sprintf("SspWebhookServiceName set to %s", envSspWebhookServiceName))
	}

	testScheme = runtime.NewScheme()
	setupApiClient()
	strategy.Init()

	// Wait to finish deployment before running any tests
	waitUntilDeployed()
})

var _ = AfterSuite(func() {
	strategy.Cleanup()
})

func expectSuccessOrNotFound(err error) {
	if err != nil && !errors.IsNotFound(err) {
		ExpectWithOffset(1, err).ToNot(HaveOccurred())
	}
}

func setupApiClient() {
	Expect(sspv1beta1.AddToScheme(testScheme)).ToNot(HaveOccurred())
	Expect(promv1.AddToScheme(testScheme)).ToNot(HaveOccurred())
	Expect(templatev1.Install(testScheme)).ToNot(HaveOccurred())
	Expect(secv1.Install(testScheme)).ToNot(HaveOccurred())
	Expect(cdiv1beta1.AddToScheme(testScheme)).ToNot(HaveOccurred())
	Expect(apiregv1.AddToScheme(testScheme)).NotTo(HaveOccurred())
	Expect(clientgoscheme.AddToScheme(testScheme)).ToNot(HaveOccurred())
	Expect(os.Setenv(kubevirtv1.KubeVirtClientGoSchemeRegistrationVersionEnvVar, "v1")).ToNot(HaveOccurred())
	Expect(kubevirtv1.AddToScheme(testScheme)).ToNot(HaveOccurred())
	Expect(instancetypev1alpha2.AddToScheme(testScheme)).ToNot(HaveOccurred())

	cfg, err := config.GetConfig()
	Expect(err).ToNot(HaveOccurred())
	apiClient, err = client.New(cfg, client.Options{Scheme: testScheme})
	Expect(err).ToNot(HaveOccurred())
	coreClient, err = kubernetes.NewForConfig(cfg)
	Expect(err).ToNot(HaveOccurred())

	Expect(openshiftroutev1.AddToScheme(testScheme)).ToNot(HaveOccurred())

	portForwarder = NewPortForwarder(cfg, coreClient.CoreV1().RESTClient())

	ctx = context.Background()
	sspListerWatcher = createSspListerWatcher(cfg)
}

func createSspListerWatcher(cfg *rest.Config) cache.ListerWatcher {
	sspGvk, err := apiutil.GVKForObject(&sspv1beta1.SSP{}, testScheme)
	Expect(err).ToNot(HaveOccurred())

	restClient, err := apiutil.RESTClientForGVK(sspGvk, false, cfg, serializer.NewCodecFactory(testScheme))
	Expect(err).ToNot(HaveOccurred())

	return cache.NewListWatchFromClient(restClient, "ssps", strategy.GetNamespace(), fields.Everything())
}

func getSsp() *sspv1beta1.SSP {
	key := client.ObjectKey{Name: strategy.GetName(), Namespace: strategy.GetNamespace()}
	foundSsp := &sspv1beta1.SSP{}
	Expect(apiClient.Get(ctx, key, foundSsp)).ToNot(HaveOccurred())
	return foundSsp
}

func getTemplateValidatorDeployment() *apps.Deployment {
	key := client.ObjectKey{Name: "virt-template-validator", Namespace: strategy.GetNamespace()}
	deployment := &apps.Deployment{}
	Expect(apiClient.Get(ctx, key, deployment)).ToNot(HaveOccurred())
	return deployment
}

func waitUntilDeployed() {
	if deploymentTimedOut {
		Fail("Timed out waiting for SSP to be in phase Deployed.")
	}

	// Set to true before waiting. In case Eventually fails,
	// it will panic and the deploymentTimedOut will be left true
	deploymentTimedOut = true
	EventuallyWithOffset(1, func() bool {
		ssp := getSsp()
		return ssp.Status.ObservedGeneration == ssp.Generation &&
			ssp.Status.Phase == lifecycleapi.PhaseDeployed
	}, env.Timeout(), time.Second).Should(BeTrue())
	deploymentTimedOut = false
}

func waitForDeletion(key client.ObjectKey, obj client.Object) {
	EventuallyWithOffset(1, func() bool {
		err := apiClient.Get(ctx, key, obj)
		return errors.IsNotFound(err)
	}, env.Timeout(), time.Second).Should(BeTrue())
}

func waitForSspDeletionIfNeeded(ssp *sspv1beta1.SSP) {
	key := client.ObjectKey{Name: ssp.Name, Namespace: ssp.Namespace}
	Eventually(func() error {
		foundSsp := &sspv1beta1.SSP{}
		err := apiClient.Get(ctx, key, foundSsp)
		if errors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if foundSsp.DeletionTimestamp != nil {
			return fmt.Errorf("waiting for SSP CR deletion")
		}
		return nil
	}, env.Timeout(), time.Second).ShouldNot(HaveOccurred())
}

func validateDeploymentExists() {
	err := apiClient.Get(ctx, client.ObjectKey{Name: sspDeploymentName, Namespace: sspDeploymentNamespace}, &apps.Deployment{})
	Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("SSP deployment does not exist under given name and namespace, check %s and %s",
		env.SspDeploymentName(), env.SspDeploymentNamespace()))
}

func createOrUpdateSsp(ssp *sspv1beta1.SSP) {
	key := client.ObjectKey{
		Name:      ssp.Name,
		Namespace: ssp.Namespace,
	}
	Eventually(func() error {
		foundSsp := &sspv1beta1.SSP{}
		err := apiClient.Get(ctx, key, foundSsp)
		if err == nil {
			isEqual := reflect.DeepEqual(foundSsp.Spec, ssp.Spec) &&
				reflect.DeepEqual(foundSsp.ObjectMeta.Annotations, ssp.ObjectMeta.Annotations) &&
				reflect.DeepEqual(foundSsp.ObjectMeta.Labels, ssp.ObjectMeta.Labels)
			if isEqual {
				return nil
			}
			foundSsp.Spec = ssp.Spec
			foundSsp.Annotations = ssp.Annotations
			foundSsp.Labels = ssp.Labels
			return apiClient.Update(ctx, foundSsp)
		}
		if errors.IsNotFound(err) {
			newSsp := &sspv1beta1.SSP{
				ObjectMeta: metav1.ObjectMeta{
					Name:        ssp.Name,
					Namespace:   ssp.Namespace,
					Annotations: ssp.Annotations,
					Labels:      ssp.Labels,
				},
				Spec: ssp.Spec,
			}
			return apiClient.Create(ctx, newSsp)
		}
		return err
	}, env.Timeout(), time.Second).ShouldNot(HaveOccurred())
}

func triggerReconciliation() {
	updateSsp(func(foundSsp *sspv1beta1.SSP) {
		if foundSsp.GetAnnotations() == nil {
			foundSsp.SetAnnotations(map[string]string{})
		}

		foundSsp.GetAnnotations()["forceReconciliation"] = ""
	})

	updateSsp(func(foundSsp *sspv1beta1.SSP) {
		delete(foundSsp.GetAnnotations(), "forceReconciliation")
	})

	// Wait a second to give time for operator to notice the change
	time.Sleep(time.Second)

	waitUntilDeployed()
}

func TestFunctional(t *testing.T) {
	var reporters []Reporter

	if qe_reporters.JunitOutput != "" {
		reporters = append(reporters, ginkgo_reporters.NewJUnitReporter(qe_reporters.JunitOutput))
	}

	if qe_reporters.Polarion.Run {
		reporters = append(reporters, &qe_reporters.Polarion)
	}

	RegisterFailHandler(Fail)
	RunSpecsWithDefaultAndCustomReporters(t, "Functional test suite", reporters)
}
