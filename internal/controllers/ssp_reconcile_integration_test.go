package controllers

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	conditionsv1 "github.com/openshift/custom-resource-status/conditions/v1"
	osconfv1 "github.com/openshift/api/config/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	lifecycleapi "kubevirt.io/controller-lifecycle-operator-sdk/api"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ssp "kubevirt.io/ssp-operator/api/v1beta3"
	"kubevirt.io/ssp-operator/internal/common"
	crd_watch "kubevirt.io/ssp-operator/internal/crd-watch"
	"kubevirt.io/ssp-operator/internal/operands"
)

// Mock operand for testing
type mockOperand struct {
	name             string
	reconcileError   error
	reconcileResults []common.ReconcileResult
}

func (m *mockOperand) Name() string {
	return m.name
}

func (m *mockOperand) WatchTypes() []operands.WatchType {
	return []operands.WatchType{}
}

func (m *mockOperand) WatchClusterTypes() []operands.WatchType {
	return []operands.WatchType{}
}

func (m *mockOperand) Reconcile(request *common.Request) ([]common.ReconcileResult, error) {
	if m.reconcileError != nil {
		return nil, m.reconcileError
	}
	return m.reconcileResults, nil
}

func (m *mockOperand) Cleanup(request *common.Request) ([]common.CleanupResult, error) {
	return []common.CleanupResult{}, nil
}

var _ = Describe("SSP Controller Reconcile Integration Tests", func() {
	var (
		ctx        context.Context
		controller *sspController
		sspObj     *ssp.SSP
		fakeClient client.Client
		scheme     *runtime.Scheme
		sspKey     types.NamespacedName
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(ssp.AddToScheme(scheme)).To(Succeed())
		Expect(v1.AddToScheme(scheme)).To(Succeed())

		sspObj = &ssp.SSP{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-ssp",
				Namespace:  "test-namespace",
				Generation: 1,
				Finalizers: []string{"ssp.kubevirt.io/finalizer"}, // Initialize with finalizer
			},
			Spec: ssp.SSPSpec{
				CommonTemplates: ssp.CommonTemplates{
					Namespace: "kubevirt",
				},
			},
			Status: ssp.SSPStatus{
				Status: lifecycleapi.Status{
					Phase: lifecycleapi.PhaseDeploying,
				},
			},
		}

		sspKey = types.NamespacedName{
			Name:      sspObj.Name,
			Namespace: sspObj.Namespace,
		}

		fakeClient = fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(sspObj).
			WithStatusSubresource(sspObj).
			Build()

		controller = &sspController{
			log:              ctrl.Log.WithName("test"),
			operands:         []operands.Operand{},
			subresourceCache: common.VersionCache{},
			topologyMode:     osconfv1.HighlyAvailableTopologyMode,
			client:           fakeClient,
			uncachedReader:   fakeClient,
			crdList:          crd_watch.New(nil),
		}
	})

	Context("When operand reconciliation fails", func() {
		It("should propagate template validator error to SSP status through full Reconcile flow", func() {
			By("Setting up a failing template validator operand")

			failingOperand := &mockOperand{
				name:           "template-validator",
				reconcileError: errors.New("Deployment kubevirt/template-validator: ImagePullBackOff"),
			}
			controller.operands = []operands.Operand{failingOperand}

			By("Calling the actual Reconcile method")

			request := ctrl.Request{NamespacedName: sspKey}
			result, err := controller.Reconcile(ctx, request)

			By("Verifying that Reconcile returns the operand error")

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("ImagePullBackOff"))
			Expect(result).To(Equal(ctrl.Result{}))

			By("Verifying that SSP status was updated with error conditions")

			updatedSSP := &ssp.SSP{}
			Expect(fakeClient.Get(ctx, sspKey, updatedSSP)).To(Succeed())

			// Check that error was propagated through handleError()
			expectedErrorMsg := "Error: Deployment kubevirt/template-validator: ImagePullBackOff"

			availableCondition := findCondition(updatedSSP.Status.Conditions, conditionsv1.ConditionAvailable)
			Expect(availableCondition).ToNot(BeNil())
			Expect(availableCondition.Status).To(Equal(v1.ConditionFalse))
			Expect(availableCondition.Message).To(Equal(expectedErrorMsg))

			progressingCondition := findCondition(updatedSSP.Status.Conditions, conditionsv1.ConditionProgressing)
			Expect(progressingCondition).ToNot(BeNil())
			Expect(progressingCondition.Status).To(Equal(v1.ConditionTrue))
			Expect(progressingCondition.Message).To(Equal(expectedErrorMsg))

			degradedCondition := findCondition(updatedSSP.Status.Conditions, conditionsv1.ConditionDegraded)
			Expect(degradedCondition).ToNot(BeNil())
			Expect(degradedCondition.Status).To(Equal(v1.ConditionTrue))
			Expect(degradedCondition.Message).To(Equal(expectedErrorMsg))

			// Phase should remain PhaseDeploying when there are errors
			Expect(updatedSSP.Status.Status.Phase).To(Equal(lifecycleapi.PhaseDeploying))
		})

		It("should handle multiple operands where first one fails", func() {
			By("Setting up multiple operands with first one failing")

			failingOperand := &mockOperand{
				name:           "common-templates",
				reconcileError: errors.New("ConfigMap kubevirt/common-templates: forbidden"),
			}

			successfulOperand := &mockOperand{
				name:           "metrics",
				reconcileError: nil,
				reconcileResults: []common.ReconcileResult{
					{Status: common.ResourceStatus{}}, // Successful result
				},
			}

			// Order matters - failing operand first
			controller.operands = []operands.Operand{failingOperand, successfulOperand}

			By("Calling Reconcile")

			request := ctrl.Request{NamespacedName: sspKey}
			result, err := controller.Reconcile(ctx, request)

			By("Verifying that reconciliation fails on first operand")

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("forbidden"))
			Expect(result).To(Equal(ctrl.Result{}))

			By("Verifying status reflects the first operand's error")

			updatedSSP := &ssp.SSP{}
			Expect(fakeClient.Get(ctx, sspKey, updatedSSP)).To(Succeed())

			condition := findCondition(updatedSSP.Status.Conditions, conditionsv1.ConditionDegraded)
			Expect(condition).ToNot(BeNil())
			Expect(condition.Status).To(Equal(v1.ConditionTrue))
			Expect(condition.Message).To(ContainSubstring("forbidden"))
		})

		It("should handle conflict errors with requeue", func() {
			By("Setting up operand that returns conflict error")

			conflictErr := apierrors.NewConflict(
				schema.GroupResource{Group: "apps", Resource: "deployments"},
				"template-validator",
				errors.New("resource version conflict"),
			)

			conflictOperand := &mockOperand{
				name:           "template-validator",
				reconcileError: conflictErr,
			}
			controller.operands = []operands.Operand{conflictOperand}

			By("Calling Reconcile")

			request := ctrl.Request{NamespacedName: sspKey}
			result, err := controller.Reconcile(ctx, request)

			By("Verifying conflict results in requeue without error")

			Expect(err).ToNot(HaveOccurred()) // Conflict errors don't return error
			Expect(result).To(Equal(ctrl.Result{Requeue: true}))

			By("Verifying status was not corrupted by conflict")

			updatedSSP := &ssp.SSP{}
			Expect(fakeClient.Get(ctx, sspKey, updatedSSP)).To(Succeed())

			// Status should remain in original PhaseDeploying state
			Expect(updatedSSP.Status.Status.Phase).To(Equal(lifecycleapi.PhaseDeploying))
		})
	})

	Context("When all operands succeed", func() {
		It("should update status through updateStatus() and reach PhaseDeployed", func() {
			By("Setting up successful operands")

			successfulOperand1 := &mockOperand{
				name:           "common-templates",
				reconcileError: nil,
				reconcileResults: []common.ReconcileResult{
					{Status: common.ResourceStatus{}}, // No errors = success
				},
			}

			successfulOperand2 := &mockOperand{
				name:           "metrics",
				reconcileError: nil,
				reconcileResults: []common.ReconcileResult{
					{Status: common.ResourceStatus{}}, // No errors = success
				},
			}

			controller.operands = []operands.Operand{successfulOperand1, successfulOperand2}

			By("Calling Reconcile")

			request := ctrl.Request{NamespacedName: sspKey}
			result, err := controller.Reconcile(ctx, request)

			By("Verifying successful reconciliation")

			Expect(err).ToNot(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			By("Verifying status shows success through updateStatus() path")

			updatedSSP := &ssp.SSP{}
			Expect(fakeClient.Get(ctx, sspKey, updatedSSP)).To(Succeed())

			// When all operands succeed, updateStatus() is called and phase becomes PhaseDeployed
			Expect(updatedSSP.Status.Status.Phase).To(Equal(lifecycleapi.PhaseDeployed))

			// Success conditions
			availableCondition := findCondition(updatedSSP.Status.Conditions, conditionsv1.ConditionAvailable)
			Expect(availableCondition).ToNot(BeNil())
			Expect(availableCondition.Status).To(Equal(v1.ConditionTrue))
			Expect(availableCondition.Message).To(Equal("All SSP resources are available"))

			progressingCondition := findCondition(updatedSSP.Status.Conditions, conditionsv1.ConditionProgressing)
			Expect(progressingCondition).ToNot(BeNil())
			Expect(progressingCondition.Status).To(Equal(v1.ConditionFalse))
			Expect(progressingCondition.Message).To(Equal("No SSP resources are progressing"))

			degradedCondition := findCondition(updatedSSP.Status.Conditions, conditionsv1.ConditionDegraded)
			Expect(degradedCondition).ToNot(BeNil())
			Expect(degradedCondition.Status).To(Equal(v1.ConditionFalse))
			Expect(degradedCondition.Message).To(Equal("No SSP resources are degraded"))
		})
	})

	Context("When CRDs are missing", func() {
		It("should handle missing CRDs through full Reconcile flow", func() {
			By("Setting up controller with missing CRDs")

			// Create a CRD watch that reports missing CRDs
			mockCrdList := &mockCrdList{
				missingCrds: []string{"virtualmachines.kubevirt.io", "datavolumes.cdi.kubevirt.io"},
			}

			controller.crdList = mockCrdList
			controller.areCrdsMissing = true

			By("Calling Reconcile")

			request := ctrl.Request{NamespacedName: sspKey}
			result, err := controller.Reconcile(ctx, request)

			By("Verifying that Reconcile handles missing CRDs")

			Expect(err).ToNot(HaveOccurred()) // Missing CRDs don't cause error, just early return
			Expect(result).To(Equal(ctrl.Result{}))

			By("Verifying status reflects missing CRDs")

			updatedSSP := &ssp.SSP{}
			Expect(fakeClient.Get(ctx, sspKey, updatedSSP)).To(Succeed())

			expectedMessage := "Required CRDs are missing: virtualmachines.kubevirt.io, datavolumes.cdi.kubevirt.io"

			availableCondition := findCondition(updatedSSP.Status.Conditions, conditionsv1.ConditionAvailable)
			Expect(availableCondition).ToNot(BeNil())
			Expect(availableCondition.Status).To(Equal(v1.ConditionFalse))
			Expect(availableCondition.Message).To(Equal(expectedMessage))
		})
	})
})

// Mock CRD list for testing missing CRDs
type mockCrdList struct {
	missingCrds []string
}

func (m *mockCrdList) CrdExists(crdName string) bool {
	for _, missing := range m.missingCrds {
		if missing == crdName {
			return false
		}
	}
	return true
}

func (m *mockCrdList) MissingCrds() []string {
	return m.missingCrds
}

// Helper function to find conditions
func findCondition(conditions []conditionsv1.Condition, condType conditionsv1.ConditionType) *conditionsv1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}
