package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/stretchr/testify/require"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	componentApi "github.com/opendatahub-io/opendatahub-operator/v2/apis/components/v1alpha1"
	dscv1 "github.com/opendatahub-io/opendatahub-operator/v2/apis/datasciencecluster/v1"
	"github.com/opendatahub-io/opendatahub-operator/v2/pkg/metadata/labels"
)

type CodeFlareTestCtx struct {
	testCtx               *testContext
	testCodeFlareInstance componentApi.CodeFlare
}

func codeflareTestSuite(t *testing.T) {
	t.Helper()

	codeflareCtx := CodeFlareTestCtx{}
	var err error
	codeflareCtx.testCtx, err = NewTestContext()
	require.NoError(t, err)

	testCtx := codeflareCtx.testCtx

	t.Run(testCtx.testDsc.Name, func(t *testing.T) {
		// creation
		t.Run("Creation of CodeFlare CR", func(t *testing.T) {
			err = codeflareCtx.testCodeFlareCreation()
			require.NoError(t, err, "error creating CodeFlare CR")
		})

		t.Run("Validate CodeFlare instance", func(t *testing.T) {
			err = codeflareCtx.validateCodeFlare()
			require.NoError(t, err, "error validating CodeFlare instance")
		})

		t.Run("Validate Ownerreferences exist", func(t *testing.T) {
			err = codeflareCtx.testOwnerReferences()
			require.NoError(t, err, "error getting all CodeFlare's Ownerreferences")
		})

		t.Run("Validate CodeFlare Ready", func(t *testing.T) {
			err = codeflareCtx.validateCodeFlareReady()
			require.NoError(t, err, "CodeFlare instance is not Ready")
		})

		// reconcile
		t.Run("Validate Controller reconcile", func(t *testing.T) {
			err = codeflareCtx.testUpdateOnCodeFlareResources()
			require.NoError(t, err, "error testing updates for CodeFlare's managed resources")
		})

		t.Run("Validate Disabling CodeFlare Component", func(t *testing.T) {
			err = codeflareCtx.testUpdateCodeFlareComponentDisabled()
			require.NoError(t, err, "error testing CodeFlare component enabled field")
		})
	})
}

func (tc *CodeFlareTestCtx) testCodeFlareCreation() error {
	err := tc.testCtx.wait(func(ctx context.Context) (bool, error) {
		key := client.ObjectKeyFromObject(tc.testCtx.testDsc)

		err := tc.testCtx.customClient.Get(ctx, key, tc.testCtx.testDsc)
		if err != nil {
			return false, fmt.Errorf("error getting resource %w", err)
		}

		tc.testCtx.testDsc.Spec.Components.CodeFlare.ManagementState = operatorv1.Managed

		switch err = tc.testCtx.customClient.Update(ctx, tc.testCtx.testDsc); {
		case err == nil:
			return true, nil
		case k8serr.IsConflict(err):
			return false, nil
		default:
			return false, fmt.Errorf("error updating resource %w", err)
		}
	})
	if err != nil {
		return fmt.Errorf("error after retry %w", err)
	}

	err = tc.testCtx.wait(func(ctx context.Context) (bool, error) {
		existingCodeFlareList := &componentApi.CodeFlareList{}

		if err := tc.testCtx.customClient.List(ctx, existingCodeFlareList); err != nil {
			return false, err
		}

		switch {
		case len(existingCodeFlareList.Items) == 1:
			tc.testCodeFlareInstance = existingCodeFlareList.Items[0]
			return true, nil
		case len(existingCodeFlareList.Items) > 1:
			return false, fmt.Errorf(
				"unexpected CodeFlare CR instances. Expected 1 , Found %v instance", len(existingCodeFlareList.Items))
		default:
			return false, nil
		}
	})

	if err != nil {
		return fmt.Errorf("unable to find CodeFlare CR instance: %w", err)
	}

	return nil
}

func (tc *CodeFlareTestCtx) validateCodeFlare() error {
	// CodeFlare spec should match the spec of CodeFlare component in DSC
	if !reflect.DeepEqual(tc.testCtx.testDsc.Spec.Components.CodeFlare.CodeFlareCommonSpec, tc.testCodeFlareInstance.Spec.CodeFlareCommonSpec) {
		err := fmt.Errorf("expected .spec for CodeFlare %v, got %v",
			tc.testCtx.testDsc.Spec.Components.CodeFlare.CodeFlareCommonSpec, tc.testCodeFlareInstance.Spec.CodeFlareCommonSpec)
		return err
	}
	return nil
}

func (tc *CodeFlareTestCtx) testOwnerReferences() error {
	if len(tc.testCodeFlareInstance.OwnerReferences) != 1 {
		return errors.New("expect CR has ownerreferences set")
	}

	// Test CodeFlare CR ownerref
	if tc.testCodeFlareInstance.OwnerReferences[0].Kind != dscKind {
		return fmt.Errorf("expected ownerreference DataScienceCluster not found. Got ownereferrence: %v",
			tc.testCodeFlareInstance.OwnerReferences[0].Kind)
	}

	// Test CodeFlare resources
	appDeployments, err := tc.testCtx.kubeClient.AppsV1().Deployments(tc.testCtx.applicationsNamespace).List(tc.testCtx.ctx, metav1.ListOptions{
		LabelSelector: labels.ODH.Component(componentApi.CodeFlareComponentName),
	})
	if err != nil {
		return fmt.Errorf("error listing component deployments %w", err)
	}
	// test any one deployment for ownerreference
	if len(appDeployments.Items) != 0 && appDeployments.Items[0].OwnerReferences[0].Kind != componentApi.CodeFlareKind {
		return fmt.Errorf("expected ownerreference not found. Got ownereferrence: %v",
			appDeployments.Items[0].OwnerReferences)
	}

	return nil
}

// Verify CodeFlare instance is in Ready phase when CodeFlare deployments are up and running.
func (tc *CodeFlareTestCtx) validateCodeFlareReady() error {
	err := wait.PollUntilContextTimeout(tc.testCtx.ctx, generalRetryInterval, componentReadyTimeout, true, func(ctx context.Context) (bool, error) {
		key := types.NamespacedName{Name: tc.testCodeFlareInstance.Name}
		CodeFlare := &componentApi.CodeFlare{}

		err := tc.testCtx.customClient.Get(ctx, key, CodeFlare)
		if err != nil {
			return false, err
		}
		return CodeFlare.Status.Phase == readyStatus, nil
	})

	if err != nil {
		return fmt.Errorf("error waiting on Ready state for CodeFlare %v: %w", tc.testCodeFlareInstance.Name, err)
	}

	err = wait.PollUntilContextTimeout(tc.testCtx.ctx, generalRetryInterval, componentReadyTimeout, true, func(ctx context.Context) (bool, error) {
		list := dscv1.DataScienceClusterList{}
		err := tc.testCtx.customClient.List(ctx, &list)
		if err != nil {
			return false, err
		}

		if len(list.Items) != 1 {
			return false, fmt.Errorf("expected 1 DataScience Cluster CR but found %v", len(list.Items))
		}

		for _, c := range list.Items[0].Status.Conditions {
			if c.Type == componentApi.CodeFlareComponentName+"Ready" {
				return c.Status == corev1.ConditionTrue, nil
			}
		}

		return false, nil
	})

	if err != nil {
		return fmt.Errorf("error waiting on Ready state for CodeFlare component in DSC: %w", err)
	}

	return nil
}

func (tc *CodeFlareTestCtx) testUpdateOnCodeFlareResources() error {
	// Test Updating CodeFlare Replicas

	appDeployments, err := tc.testCtx.kubeClient.AppsV1().Deployments(tc.testCtx.applicationsNamespace).List(tc.testCtx.ctx, metav1.ListOptions{
		LabelSelector: labels.PlatformPartOf + "=" + strings.ToLower(tc.testCodeFlareInstance.Kind),
	})
	if err != nil {
		return err
	}

	if len(appDeployments.Items) != 1 {
		return fmt.Errorf("error getting deployment for component %s", tc.testCodeFlareInstance.Name)
	}

	const expectedReplica int32 = 2 // from 1 to 2

	testDeployment := appDeployments.Items[0]
	patchedReplica := &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testDeployment.Name,
			Namespace: testDeployment.Namespace,
		},
		Spec: autoscalingv1.ScaleSpec{
			Replicas: expectedReplica,
		},
		Status: autoscalingv1.ScaleStatus{},
	}
	updatedDep, err := tc.testCtx.kubeClient.AppsV1().Deployments(tc.testCtx.applicationsNamespace).UpdateScale(tc.testCtx.ctx,
		testDeployment.Name, patchedReplica, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error patching component resources : %w", err)
	}
	if updatedDep.Spec.Replicas != patchedReplica.Spec.Replicas {
		return fmt.Errorf("failed to patch replicas : expect to be %v but got %v", patchedReplica.Spec.Replicas, updatedDep.Spec.Replicas)
	}

	// Sleep for 20 seconds to allow the operator to reconcile
	// we expect it should not revert back to original value because of AllowList
	time.Sleep(2 * generalRetryInterval)
	reconciledDep, err := tc.testCtx.kubeClient.AppsV1().Deployments(tc.testCtx.applicationsNamespace).Get(tc.testCtx.ctx, testDeployment.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting component resource after reconcile: %w", err)
	}
	if *reconciledDep.Spec.Replicas != expectedReplica {
		return fmt.Errorf("failed to revert back replicas : expect to be %v but got %v", expectedReplica, *reconciledDep.Spec.Replicas)
	}

	return nil
}

func (tc *CodeFlareTestCtx) testUpdateCodeFlareComponentDisabled() error {
	// Test Updating CodeFlare to be disabled
	var codeflareDeploymentName string

	if tc.testCtx.testDsc.Spec.Components.CodeFlare.ManagementState == operatorv1.Managed {
		appDeployments, err := tc.testCtx.kubeClient.AppsV1().Deployments(tc.testCtx.applicationsNamespace).List(tc.testCtx.ctx, metav1.ListOptions{
			LabelSelector: labels.ODH.Component(componentApi.CodeFlareComponentName),
		})
		if err != nil {
			return fmt.Errorf("error getting enabled component %v", componentApi.CodeFlareComponentName)
		}
		if len(appDeployments.Items) > 0 {
			codeflareDeploymentName = appDeployments.Items[0].Name
			if appDeployments.Items[0].Status.ReadyReplicas == 0 {
				return fmt.Errorf("error getting enabled component: %s its deployment 'ReadyReplicas'", codeflareDeploymentName)
			}
		}
	} else {
		return errors.New("CodeFlare spec should be in 'enabled: true' state in order to perform test")
	}

	// Disable component CodeFlare
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// refresh DSC instance in case it was updated during the reconcile
		err := tc.testCtx.customClient.Get(tc.testCtx.ctx, types.NamespacedName{Name: tc.testCtx.testDsc.Name}, tc.testCtx.testDsc)
		if err != nil {
			return fmt.Errorf("error getting resource %w", err)
		}
		// Disable the Component
		tc.testCtx.testDsc.Spec.Components.CodeFlare.ManagementState = operatorv1.Removed

		// Try to update
		err = tc.testCtx.customClient.Update(tc.testCtx.ctx, tc.testCtx.testDsc)
		// Return err itself here (not wrapped inside another error)
		// so that RetryOnConflict can identify it correctly.
		if err != nil {
			return fmt.Errorf("error updating component from 'enabled: true' to 'enabled: false': %w", err)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("error after retry %w", err)
	}

	if err = tc.testCtx.wait(func(ctx context.Context) (bool, error) {
		// Verify CodeFlare CR is deleted
		CodeFlare := &componentApi.CodeFlare{}
		err = tc.testCtx.customClient.Get(ctx, client.ObjectKey{Name: tc.testCodeFlareInstance.Name}, CodeFlare)
		return k8serr.IsNotFound(err), nil
	}); err != nil {
		return fmt.Errorf("component CodeFlare is disabled, should not get the CodeFlare CR %v", tc.testCodeFlareInstance.Name)
	}

	// Sleep for 20 seconds to allow the operator to reconcile
	time.Sleep(2 * generalRetryInterval)
	_, err = tc.testCtx.kubeClient.AppsV1().Deployments(tc.testCtx.applicationsNamespace).Get(tc.testCtx.ctx, codeflareDeploymentName, metav1.GetOptions{})
	if err != nil {
		if k8serr.IsNotFound(err) {
			return nil // correct result: should not find deployment after we disable it already
		}
		return fmt.Errorf("error getting component resource after reconcile: %w", err)
	}
	return fmt.Errorf("component %v is disabled, should not get its deployment %v from NS %v any more",
		componentApi.CodeFlareKind,
		codeflareDeploymentName,
		tc.testCtx.applicationsNamespace)
}
