// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package crdmanagement

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	. "github.com/Azure/azure-service-operator/v2/internal/logging"

	"github.com/go-logr/logr"
	"github.com/rotisserie/eris"
	"golang.org/x/exp/maps"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrlleader "sigs.k8s.io/controller-runtime/pkg/leaderelection"
	"sigs.k8s.io/yaml"

	"github.com/Azure/azure-service-operator/v2/internal/util/kubeclient"
	"github.com/Azure/azure-service-operator/v2/internal/util/match"
)

// ServiceOperatorVersionLabelOld is the label the CRDs have on them containing the ASO version. This value must match the value
// injected by config/crd/labels.yaml
const (
	ServiceOperatorVersionLabelOld = "serviceoperator.azure.com/version"
	ServiceOperatorVersionLabel    = "app.kubernetes.io/version"
	ServiceOperatorAppLabel        = "app.kubernetes.io/name"
	ServiceOperatorAppValue        = "azure-service-operator"
)

const CRDLocation = "crds"

const certMgrInjectCAFromAnnotation = "cert-manager.io/inject-ca-from"

type LeaderElector struct {
	Elector       *leaderelection.LeaderElector
	LeaseAcquired *sync.WaitGroup
	LeaseReleased *sync.WaitGroup
}

// NewLeaderElector creates a new LeaderElector
func NewLeaderElector(
	k8sConfig *rest.Config,
	log logr.Logger,
	ctrlOptions ctrl.Options,
	mgr ctrl.Manager,
) (*LeaderElector, error) {
	resourceLock, err := ctrlleader.NewResourceLock(
		k8sConfig,
		mgr,
		ctrlleader.Options{
			LeaderElection:             ctrlOptions.LeaderElection,
			LeaderElectionResourceLock: ctrlOptions.LeaderElectionResourceLock,
			LeaderElectionID:           ctrlOptions.LeaderElectionID,
		})
	if err != nil {
		return nil, err
	}

	log = log.WithName("crdManagementLeaderElector")
	leaseAcquiredWait := &sync.WaitGroup{}
	leaseAcquiredWait.Add(1)
	leaseReleasedWait := &sync.WaitGroup{}
	leaseReleasedWait.Add(1)

	var leaderContext context.Context
	var leaderContextLock sync.Mutex // used to ensure reads/writes of leaderContext are safe

	leaderElector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:          resourceLock,
		LeaseDuration: *ctrlOptions.LeaseDuration,
		RenewDeadline: *ctrlOptions.RenewDeadline,
		RetryPeriod:   *ctrlOptions.RetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				log.V(Status).Info("Elected leader")
				leaseAcquiredWait.Done()

				leaderContextLock.Lock()
				leaderContext = ctx
				leaderContextLock.Unlock()
			},
			OnStoppedLeading: func() {
				leaseReleasedWait.Done()

				// Cache the channel from current leader context so it can't be changed while we're using it
				leaderContextLock.Lock()
				lc := leaderContext
				leaderContextLock.Unlock()

				exitCode := 1
				if lc != nil {
					select {
					case <-lc.Done():
						exitCode = 0 // done is closed
					default:
					}
				}

				if exitCode == 0 {
					log.V(Status).Info("Lost leader due to cooperative lease release")
				} else {
					log.V(Status).Info("Lost leader")
				}
				os.Exit(exitCode)
			},
		},
		ReleaseOnCancel: ctrlOptions.LeaderElectionReleaseOnCancel,
		Name:            ctrlOptions.LeaderElectionID,
	})
	if err != nil {
		return nil, err
	}

	return &LeaderElector{
		Elector:       leaderElector,
		LeaseAcquired: leaseAcquiredWait,
		LeaseReleased: leaseReleasedWait,
	}, nil
}

type Manager struct {
	logger         logr.Logger
	kubeClient     kubeclient.Client
	leaderElection *LeaderElector

	crds []apiextensions.CustomResourceDefinition
}

// NewManager creates a new CRD manager.
// The leaderElection argument is optional, but strongly recommended.
func NewManager(logger logr.Logger, kubeClient kubeclient.Client, leaderElection *LeaderElector) *Manager {
	return &Manager{
		logger:         logger,
		kubeClient:     kubeClient,
		leaderElection: leaderElection,
	}
}

// ListCRDs lists ASO CRDs.
// This accepts a list rather than returning one to allow re-using the same list object (they're large and having multiple)
// copies of the collection results in huge memory usage.
func (m *Manager) ListCRDs(ctx context.Context, list *apiextensions.CustomResourceDefinitionList) error {
	// Clear the existing list, if there is one.
	list.Items = nil
	list.Continue = ""
	list.ResourceVersion = ""

	selector := labels.NewSelector()
	requirement, err := labels.NewRequirement(ServiceOperatorAppLabel, selection.Equals, []string{ServiceOperatorAppValue})
	if err != nil {
		return err
	}
	selector = selector.Add(*requirement)

	match := client.MatchingLabelsSelector{
		Selector: selector,
	}

	err = m.kubeClient.List(ctx, list, match)
	if err != nil {
		return eris.Wrapf(err, "failed to list CRDs")
	}

	for _, crd := range list.Items {
		m.logger.V(Verbose).Info("Found an existing CRD", "CRD", crd.Name)
	}

	return nil
}

func (m *Manager) LoadOperatorCRDs(path string, podNamespace string) ([]apiextensions.CustomResourceDefinition, error) {
	if len(m.crds) > 0 {
		// Nothing to do as they're already loaded. Pod has to restart for them to change
		return m.crds, nil
	}

	crds, err := m.loadCRDs(path)
	if err != nil {
		return nil, err
	}
	crds = m.fixCRDNamespaceRefs(crds, podNamespace)

	m.crds = crds
	return crds, nil
}

// FindMatchingCRDs finds the CRDs in "goal" that are in "existing" AND compare as equal according to the comparators with
// the corresponding CRD in "goal"
func (m *Manager) FindMatchingCRDs(
	existing []apiextensions.CustomResourceDefinition,
	goal []apiextensions.CustomResourceDefinition,
	comparators ...func(a apiextensions.CustomResourceDefinition, b apiextensions.CustomResourceDefinition) bool,
) map[string]apiextensions.CustomResourceDefinition {
	matching := make(map[string]apiextensions.CustomResourceDefinition)

	// Build a map so lookup is faster
	existingCRDs := make(map[string]apiextensions.CustomResourceDefinition, len(existing))
	for _, crd := range existing {
		existingCRDs[crd.Name] = crd
	}

	// Every goal CRD should exist and match an existing one
	for _, goalCRD := range goal {

		// Note that if the CRD is not found, we will get back a default initialized CRD.
		// We run the comparators on that as they may match, especially if the comparator is something like
		// "specs are not equal"
		existingCRD := existingCRDs[goalCRD.Name]

		// Deepcopy to ensure that modifications below don't persist
		existingCRD = *existingCRD.DeepCopy()
		goalCRD = *goalCRD.DeepCopy()

		equal := true
		for _, c := range comparators {
			if !c(existingCRD, goalCRD) { //nolint: gosimple
				equal = false
				break
			}
		}

		if equal {
			matching[goalCRD.Name] = goalCRD
		}
	}

	return matching
}

// FindNonMatchingCRDs finds the CRDs in "goal" that are not in "existing" OR are in "existing" but mismatch with the "goal"
// based on the comparator functions.
func (m *Manager) FindNonMatchingCRDs(
	existing []apiextensions.CustomResourceDefinition,
	goal []apiextensions.CustomResourceDefinition,
	comparators ...func(a apiextensions.CustomResourceDefinition, b apiextensions.CustomResourceDefinition) bool,
) map[string]apiextensions.CustomResourceDefinition {
	// Just invert the comparators and call FindMatchingCRDs
	invertedComparators := make([]func(a apiextensions.CustomResourceDefinition, b apiextensions.CustomResourceDefinition) bool, 0, len(comparators))
	for _, c := range comparators {
		c := c
		invertedComparators = append(
			invertedComparators,
			func(a apiextensions.CustomResourceDefinition, b apiextensions.CustomResourceDefinition) bool {
				return !c(a, b)
			})
	}

	return m.FindMatchingCRDs(existing, goal, invertedComparators...)
}

// DetermineCRDsToInstallOrUpgrade examines the set of goal CRDs and installed CRDs to determine the set which should
// be installed or upgraded.
func (m *Manager) DetermineCRDsToInstallOrUpgrade(
	goalCRDs []apiextensions.CustomResourceDefinition,
	existingCRDs []apiextensions.CustomResourceDefinition,
	patterns string,
) ([]*CRDInstallationInstruction, error) {
	m.logger.V(Info).Info("Goal CRDs", "count", len(goalCRDs))
	m.logger.V(Info).Info("Existing CRDs", "count", len(existingCRDs))

	// Filter the goal CRDs to only those goal CRDs that match an already installed CRD
	resultMap := make(map[string]*CRDInstallationInstruction, len(goalCRDs))
	for _, crd := range goalCRDs {
		resultMap[crd.Name] = &CRDInstallationInstruction{
			CRD: crd,
			// Assumption to begin with is that the CRD is excluded. This will get updated later if it's matched.
			FilterResult: Excluded,
			FilterReason: fmt.Sprintf("%q was not matched by CRD pattern and did not already exist in cluster", makeMatchString(crd)),
			DiffResult:   NoDifference,
		}
	}

	m.filterCRDsByExisting(existingCRDs, resultMap)
	err := m.filterCRDsByPatterns(patterns, resultMap)
	if err != nil {
		return nil, err
	}

	// Prealloc false positive: https://github.com/alexkohler/prealloc/issues/16
	//nolint:prealloc
	var filteredGoalCRDs []apiextensions.CustomResourceDefinition
	for _, result := range resultMap {
		if result.FilterResult == Excluded {
			continue
		}

		filteredGoalCRDs = append(filteredGoalCRDs, result.CRD)
	}

	goalCRDsWithDifferentVersion := m.FindNonMatchingCRDs(existingCRDs, filteredGoalCRDs, VersionEqual)
	goalCRDsWithDifferentSpec := m.FindNonMatchingCRDs(existingCRDs, filteredGoalCRDs, SpecEqual)

	// The same CRD may be in both sets, but we don't want to include it in the results twice
	for name := range goalCRDsWithDifferentSpec {
		result, ok := resultMap[name]
		if !ok {
			return nil, eris.Errorf("Couldn't find goal CRD %q. This is unexpected!", name)
		}

		result.DiffResult = SpecDifferent
	}
	for name := range goalCRDsWithDifferentVersion {
		result, ok := resultMap[name]
		if !ok {
			return nil, eris.Errorf("Couldn't find goal CRD %q. This is unexpected!", name)
		}

		result.DiffResult = VersionDifferent
	}

	// Collapse result to a slice
	results := maps.Values(resultMap)
	return results, nil
}

func (m *Manager) applyCRDs(
	ctx context.Context,
	goalCRDs []apiextensions.CustomResourceDefinition,
	instructions []*CRDInstallationInstruction,
	options Options,
) error {
	instructionsToApply := m.filterInstallationInstructions(instructions, true)

	if len(instructionsToApply) == 0 {
		m.logger.V(Status).Info("Successfully reconciled CRDs because there were no CRDs to update.")
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if m.leaderElection != nil {
		m.logger.V(Status).Info("Acquiring leader lock...")
		go m.leaderElection.Elector.Run(ctx)
		m.leaderElection.LeaseAcquired.Wait() // Wait for lease to be acquired

		// If lease was acquired we always want to wait til it's released, but defers run in LIFO order
		// so we need to make sure that the ctx is cancelled first here
		defer func() {
			cancel()
			m.leaderElection.LeaseReleased.Wait()
		}()

		// Double-checked locking, we need to make sure once we have the lock there's still work to do, as it may
		// already have been done while we were waiting for the lock.
		m.logger.V(Status).Info("Double-checked locking - ensure there's still CRDs to apply...")
		err := m.ListCRDs(ctx, options.ExistingCRDs)
		if err != nil {
			return eris.Wrap(err, "failed to list current CRDs")
		}
		instructions, err = m.DetermineCRDsToInstallOrUpgrade(goalCRDs, options.ExistingCRDs.Items, options.CRDPatterns)
		if err != nil {
			return eris.Wrap(err, "failed to determine CRDs to apply")
		}
		instructionsToApply = m.filterInstallationInstructions(instructions, false)
		if len(instructionsToApply) == 0 {
			m.logger.V(Status).Info("Successfully reconciled CRDs because there were no CRDs to update.")
			return nil
		}
	}

	m.logger.V(Status).Info("Will apply CRDs", "count", len(instructionsToApply))

	i := 0
	for _, instruction := range instructionsToApply {
		instruction := instruction

		i += 1
		toApply := &apiextensions.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name: instruction.CRD.Name,
			},
		}
		m.logger.V(Verbose).Info(
			"Applying CRD",
			"progress", fmt.Sprintf("%d/%d", i, len(instructionsToApply)),
			"crd", instruction.CRD.Name)

		result, err := controllerutil.CreateOrUpdate(ctx, m.kubeClient, toApply, func() error {
			resourceVersion := toApply.ResourceVersion
			*toApply = instruction.CRD
			toApply.ResourceVersion = resourceVersion

			return nil
		})
		if err != nil {
			return eris.Wrapf(err, "failed to apply CRD %s", instruction.CRD.Name)
		}

		m.logger.V(Debug).Info("Successfully applied CRD", "name", instruction.CRD.Name, "result", result)
	}

	// Cancel the context, and wait for the lease to complete
	if m.leaderElection != nil {
		m.logger.V(Info).Info("Giving up leadership lease")
		cancel()
		m.leaderElection.LeaseReleased.Wait()
	}

	// If we make it to here, we have successfully updated all the CRDs we needed to. We need to kill the pod and let it restart so
	// that the new shape CRDs can be reconciled.
	m.logger.V(Status).Info("Restarting operator pod after updating CRDs", "count", len(instructionsToApply))
	os.Exit(0)

	// Will never get here
	return nil
}

type Options struct {
	Path         string
	Namespace    string
	CRDPatterns  string
	ExistingCRDs *apiextensions.CustomResourceDefinitionList
}

func (m *Manager) Install(ctx context.Context, options Options) error {
	goalCRDs, err := m.LoadOperatorCRDs(options.Path, options.Namespace)
	if err != nil {
		return eris.Wrap(err, "failed to load CRDs from disk")
	}

	installationInstructions, err := m.DetermineCRDsToInstallOrUpgrade(goalCRDs, options.ExistingCRDs.Items, options.CRDPatterns)
	if err != nil {
		return eris.Wrap(err, "failed to determine CRDs to apply")
	}

	included := IncludedCRDs(installationInstructions)
	if len(included) == 0 {
		return eris.New("No existing CRDs in cluster and no --crd-pattern specified")
	}

	// Note that this step will restart the pod when it succeeds
	// if any CRDs were applied.
	err = m.applyCRDs(ctx, goalCRDs, installationInstructions, options)
	if err != nil {
		return eris.Wrap(err, "failed to apply CRDs")
	}

	return nil
}

func (m *Manager) loadCRDs(path string) ([]apiextensions.CustomResourceDefinition, error) {
	// Expectation is that every file in this folder is a CRD
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, eris.Wrapf(err, "failed to read directory %s", path)
	}

	results := make([]apiextensions.CustomResourceDefinition, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() {
			continue // Ignore directories
		}

		filePath := filepath.Join(path, entry.Name())
		var content []byte
		content, err = os.ReadFile(filePath)
		if err != nil {
			return nil, eris.Wrapf(err, "failed to read %s", filePath)
		}

		crd := apiextensions.CustomResourceDefinition{}
		err = yaml.Unmarshal(content, &crd)
		if err != nil {
			return nil, eris.Wrapf(err, "failed to unmarshal %s to CRD", filePath)
		}

		m.logger.V(Verbose).Info("Loaded CRD", "crdPath", filePath, "name", crd.Name)
		results = append(results, crd)
	}

	return results, nil
}

func (m *Manager) filterInstallationInstructions(instructions []*CRDInstallationInstruction, log bool) []*CRDInstallationInstruction {
	var instructionsToApply []*CRDInstallationInstruction

	for _, item := range instructions {
		apply, reason := item.ShouldApply()
		if apply {
			instructionsToApply = append(instructionsToApply, item)
			if log {
				m.logger.V(Verbose).Info(
					"Will update CRD",
					"crd", item.CRD.Name,
					"diffResult", item.DiffResult,
					"filterReason", item.FilterReason,
					"reason", reason)
			}
		} else {
			if log {
				m.logger.V(Verbose).Info(
					"Will NOT update CRD",
					"crd", item.CRD.Name,
					"reason", reason)
			}
		}
	}

	return instructionsToApply
}

func (m *Manager) fixCRDNamespaceRefs(crds []apiextensions.CustomResourceDefinition, namespace string) []apiextensions.CustomResourceDefinition {
	results := make([]apiextensions.CustomResourceDefinition, 0, len(crds))

	for _, crd := range crds {
		crd = fixCRDNamespace(crd, namespace)
		results = append(results, crd)
	}

	return results
}

func (m *Manager) filterCRDsByExisting(existingCRDs []apiextensions.CustomResourceDefinition, resultMap map[string]*CRDInstallationInstruction) {
	for _, crd := range existingCRDs {
		result, ok := resultMap[crd.Name]
		if !ok {
			m.logger.V(Status).Info("Found existing CRD for which no goal CRD exists. This is unexpected!", "existing", makeMatchString(crd))
			continue
		}

		result.FilterResult = MatchedExistingCRD
		result.FilterReason = fmt.Sprintf("A CRD named %q was already installed, considering that existing CRD for update", makeMatchString(crd))
	}
}

func (m *Manager) filterCRDsByPatterns(patterns string, resultMap map[string]*CRDInstallationInstruction) error {
	if patterns == "" {
		return nil
	}

	matcher := match.NewStringMatcher(patterns)

	for _, goal := range resultMap {
		matchString := makeMatchString(goal.CRD)
		matchResult := matcher.Matches(matchString)
		if matchResult.Matched {
			goal.FilterResult = MatchedPattern
			goal.FilterReason = fmt.Sprintf("CRD named %q matched pattern %q", makeMatchString(goal.CRD), matchResult.MatchingPattern)
		}
	}

	err := matcher.WasMatched()
	if err != nil {
		return err
	}

	return nil
}

// fixCRDNamespace fixes up namespace references in the CRD to match the provided namespace.
// This could in theory be done with a string replace across the JSON representation of the CRD, but that's risky given
// we don't know what else might have the "azureserviceoperator-system" string in it. Instead, we hardcode specific places
// we know need to be fixed up. This is more brittle in the face of namespace additions but has the advantage of guaranteeing
// that we can't break our own CRDs with a string replace gone awry.
func fixCRDNamespace(crd apiextensions.CustomResourceDefinition, namespace string) apiextensions.CustomResourceDefinition {
	result := crd.DeepCopy()

	// Set spec.conversion.webhook.clientConfig.service.namespace
	if result.Spec.Conversion != nil &&
		result.Spec.Conversion.Webhook != nil &&
		result.Spec.Conversion.Webhook.ClientConfig != nil &&
		result.Spec.Conversion.Webhook.ClientConfig.Service != nil {
		result.Spec.Conversion.Webhook.ClientConfig.Service.Namespace = namespace
	}

	// Set cert-manager.io/inject-ca-from
	if len(result.Annotations) > 0 {
		if injectCAFrom, ok := result.Annotations[certMgrInjectCAFromAnnotation]; ok {
			split := strings.Split(injectCAFrom, "/")
			if len(split) == 2 {
				result.Annotations[certMgrInjectCAFromAnnotation] = fmt.Sprintf("%s/%s", namespace, split[1])
			}
		}
	}

	return *result
}

func ignoreCABundle(a apiextensions.CustomResourceDefinition) apiextensions.CustomResourceDefinition {
	if a.Spec.Conversion != nil && a.Spec.Conversion.Webhook != nil &&
		a.Spec.Conversion.Webhook.ClientConfig != nil {
		a.Spec.Conversion.Webhook.ClientConfig.CABundle = nil
	}

	return a
}

func ignoreConversionWebhook(a apiextensions.CustomResourceDefinition) apiextensions.CustomResourceDefinition {
	if a.Spec.Conversion != nil && a.Spec.Conversion.Webhook != nil {
		a.Spec.Conversion.Webhook = nil
	}

	return a
}

func SpecEqual(a apiextensions.CustomResourceDefinition, b apiextensions.CustomResourceDefinition) bool {
	a = ignoreCABundle(a)
	b = ignoreCABundle(b)

	return reflect.DeepEqual(a.Spec, b.Spec)
}

func SpecEqualIgnoreConversionWebhook(a apiextensions.CustomResourceDefinition, b apiextensions.CustomResourceDefinition) bool {
	a = ignoreConversionWebhook(a)
	b = ignoreConversionWebhook(b)

	return reflect.DeepEqual(a.Spec, b.Spec)
}

func VersionEqual(a apiextensions.CustomResourceDefinition, b apiextensions.CustomResourceDefinition) bool {
	if a.Labels == nil && b.Labels == nil {
		return true
	}

	if a.Labels == nil || b.Labels == nil {
		return false
	}

	aVersion, aOk := a.Labels[ServiceOperatorVersionLabel]
	bVersion, bOk := b.Labels[ServiceOperatorVersionLabel]

	if !aOk && !bOk {
		return true
	}

	return aVersion == bVersion
}
