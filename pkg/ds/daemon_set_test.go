package ds

import (
	"fmt"
	"testing"
	"time"

	"github.com/square/p2/pkg/kp"
	"github.com/square/p2/pkg/util"

	"github.com/square/p2/pkg/kp/dsstore"
	"github.com/square/p2/pkg/kp/dsstore/dsstoretest"
	"github.com/square/p2/pkg/kp/kptest"
	"github.com/square/p2/pkg/labels"
	"github.com/square/p2/pkg/logging"
	"github.com/square/p2/pkg/manifest"
	"github.com/square/p2/pkg/types"

	. "github.com/anthonybishopric/gotcha"
	ds_fields "github.com/square/p2/pkg/ds/fields"
	klabels "k8s.io/kubernetes/pkg/labels"
)

func scheduledPods(t *testing.T, ds *daemonSet) []labels.Labeled {
	selector := klabels.Everything().Add(DSIDLabel, klabels.EqualsOperator, []string{ds.ID().String()})
	labeled, err := ds.applicator.GetMatches(selector, labels.POD)
	Assert(t).IsNil(err, "expected no error matching pods")
	return labeled
}

func waitForNodes(
	t *testing.T,
	ds DaemonSet,
	desired int,
	desiresErrCh <-chan error,
	changesErrCh <-chan error,
) int {
	timeout := time.After(1 * time.Second)
	podLocations, err := ds.CurrentPods()
	Assert(t).IsNil(err, "expected no error getting pod locations")
	timedOut := false

	// This for loop runs until you either time out or len(podLocations) == desired
	// then return the length of whatever ds.CurrentNodes() is
	for len(podLocations) != desired && !timedOut {
		select {
		case <-time.Tick(100 * time.Millisecond):
			// Also check for errors
			var err error
			podLocations, err = ds.CurrentPods()
			Assert(t).IsNil(err, "expected no error getting pod locations nodes")

			select {
			case err = <-desiresErrCh:
				Assert(t).IsNil(err, "expected no error watches desires")
			case err = <-changesErrCh:
				Assert(t).IsNil(err, "expected no error watching for changes")
			default:
			}

		case <-timeout:
			timedOut = true
		}
	}
	return len(podLocations)
}

// Watches for changes to daemon sets and sends update and delete signals
// since these are unit tests and have little daemon sets, we will watch
// the entire tree for each daemon set for now
func watchDSChanges(
	t *testing.T,
	ds *daemonSet,
	dsStore dsstore.Store,
	quitCh <-chan struct{},
	updatedCh chan<- ds_fields.DaemonSet,
	deletedCh chan<- struct{},
) <-chan error {
	errCh := make(chan error)
	changesCh := dsStore.Watch(quitCh)

	go func() {
		defer close(errCh)

		for {
			var watched dsstore.WatchedDaemonSets

			// Get some changes
			select {
			case watched = <-changesCh:
			case <-quitCh:
				return
			}

			if watched.Err != nil {
				errCh <- util.Errorf("Error occured when watching daemon sets: %v", watched.Err)
			}

			// Signal daemon set when changes have been made,
			// creations are handled when WatchDesires is called, so ignore them here
			for _, changedDS := range watched.Updated {
				if ds.ID() == changedDS.ID {
					ds.logger.NoFields().Infof("Watched daemon set get updated: %v", *changedDS)
					updatedCh <- *changedDS
				}
			}
			for _, changedDS := range watched.Deleted {
				if ds.ID() == changedDS.ID {
					ds.logger.NoFields().Infof("Watched daemon set get deleted: %v", changedDS)
					deletedCh <- struct{}{}
				}
			}
		}
	}()
	return errCh
}

// TestSchedule checks consecutive scheduling and unscheduling for:
//	- creation of a daemon set
// 	- different node selectors
//	- changes to nodes allocations
// 	- mutations to a daemon set
//	- deleting a daemon set
func TestSchedule(t *testing.T) {
	//
	// Setup fixture and schedule a pod
	//
	dsStore := dsstoretest.NewFake()

	podID := types.PodID("testPod")
	minHealth := 0
	clusterName := ds_fields.ClusterName("some_name")

	manifestBuilder := manifest.NewBuilder()
	manifestBuilder.SetID(podID)
	podManifest := manifestBuilder.GetManifest()

	nodeSelector := klabels.Everything().Add("nodeQuality", klabels.EqualsOperator, []string{"good"})

	dsData, err := dsStore.Create(podManifest, minHealth, clusterName, nodeSelector, podID)
	Assert(t).IsNil(err, "expected no error creating request")

	kpStore := kptest.NewFakePodStore(make(map[kptest.FakePodStoreKey]manifest.Manifest), make(map[string]kp.WatchResult))
	applicator := labels.NewFakeApplicator()

	ds := New(
		dsData,
		dsStore,
		kpStore,
		applicator,
		logging.DefaultLogger,
	).(*daemonSet)

	scheduled := scheduledPods(t, ds)
	Assert(t).AreEqual(len(scheduled), 0, "expected no pods to have been labeled")
	manifestResults, _, err := kpStore.AllPods(kp.INTENT_TREE)
	if err != nil {
		t.Fatalf("Unable to get all pods from pod store: %v", err)
	}
	Assert(t).AreEqual(len(manifestResults), 0, "expected no manifests to have been scheduled")

	err = applicator.SetLabel(labels.NODE, "node1", "nodeQuality", "bad")
	Assert(t).IsNil(err, "expected no error labeling node1")
	err = applicator.SetLabel(labels.NODE, "node2", "nodeQuality", "good")
	Assert(t).IsNil(err, "expected no error labeling node2")

	//
	// Adds a watch that will automatically send a signal when a change was made
	// to the daemon set
	//
	quitCh := make(chan struct{})
	updatedCh := make(chan ds_fields.DaemonSet)
	deletedCh := make(chan struct{})
	nodesChangedCh := make(chan struct{})
	defer close(quitCh)
	defer close(updatedCh)
	defer close(deletedCh)
	defer close(nodesChangedCh)
	desiresErrCh := ds.WatchDesires(quitCh, updatedCh, deletedCh, nodesChangedCh)
	changesErrCh := watchDSChanges(t, ds, dsStore, quitCh, updatedCh, deletedCh)

	//
	// Verify that the pod has been scheduled
	//
	numNodes := waitForNodes(t, ds, 1, desiresErrCh, changesErrCh)
	Assert(t).AreEqual(numNodes, 1, "took too long to schedule")

	scheduled = scheduledPods(t, ds)
	Assert(t).AreEqual(len(scheduled), 1, "expected a node to have been labeled")
	Assert(t).AreEqual(scheduled[0].ID, "node2/testPod", "expected node labeled with the daemon set's id")

	// Verify that the scheduled pod is correct
	manifestResults, _, err = kpStore.AllPods(kp.INTENT_TREE)
	if err != nil {
		t.Fatalf("Unable to get all pods from pod store: %v", err)
	}
	for _, val := range manifestResults {
		Assert(t).AreEqual(val.Path, "intent/node2/testPod", "expected manifest scheduled on the right node")
		Assert(t).AreEqual(string(val.Manifest.ID()), "testPod", "expected manifest with correct ID")
	}

	//
	// Add 10 good nodes and 10 bad nodes then verify
	//
	for i := 0; i < 10; i++ {
		nodeName := fmt.Sprintf("good_node%v", i)
		err := applicator.SetLabel(labels.NODE, nodeName, "nodeQuality", "good")
		Assert(t).IsNil(err, "expected no error labeling node")
	}

	for i := 0; i < 10; i++ {
		nodeName := fmt.Sprintf("bad_node%v", i)
		err := applicator.SetLabel(labels.NODE, nodeName, "nodeQuality", "bad")
		Assert(t).IsNil(err, "expected no error labeling node")
	}

	// Manually signal that nodes have been changed since a watch for that has
	// not been implemented yet
	nodesChangedCh <- struct{}{}

	numNodes = waitForNodes(t, ds, 11, desiresErrCh, changesErrCh)
	Assert(t).AreEqual(numNodes, 11, "took too long to schedule")

	scheduled = scheduledPods(t, ds)
	Assert(t).AreEqual(len(scheduled), 11, "expected a lot of nodes to have been labeled")

	//
	// Add a node with the labels nodeQuality=good and cherry=pick
	//
	err = applicator.SetLabel(labels.NODE, "nodeOk", "nodeQuality", "good")
	Assert(t).IsNil(err, "expected no error labeling nodeOk")
	err = applicator.SetLabel(labels.NODE, "nodeOk", "cherry", "pick")
	Assert(t).IsNil(err, "expected no error labeling nodeOk")
	fmt.Println(applicator.GetLabels(labels.NODE, "nodeOk"))

	// Manually signal that nodes have been changed since a watch for that has
	// not been implemented yet
	nodesChangedCh <- struct{}{}

	numNodes = waitForNodes(t, ds, 12, desiresErrCh, changesErrCh)
	Assert(t).AreEqual(numNodes, 12, "took too long to schedule")

	// Schedule only a node that is both nodeQuality=good and cherry=pick
	mutator := func(dsToChange ds_fields.DaemonSet) (ds_fields.DaemonSet, error) {
		dsToChange.NodeSelector = klabels.Everything().
			Add("nodeQuality", klabels.EqualsOperator, []string{"good"}).
			Add("cherry", klabels.EqualsOperator, []string{"pick"})
		return dsToChange, nil
	}
	_, err = dsStore.MutateDS(ds.ID(), mutator)
	Assert(t).IsNil(err, "Unxpected error trying to mutate daemon set")

	numNodes = waitForNodes(t, ds, 1, desiresErrCh, changesErrCh)
	Assert(t).AreEqual(numNodes, 1, "took too long to schedule")

	// Verify that the scheduled pod is correct
	manifestResults, _, err = kpStore.AllPods(kp.INTENT_TREE)
	if err != nil {
		t.Fatalf("Unable to get all pods from pod store: %v", err)
	}
	for _, val := range manifestResults {
		Assert(t).AreEqual(val.Path, "intent/nodeOk/testPod", "expected manifest scheduled on the right node")
		Assert(t).AreEqual(string(val.Manifest.ID()), "testPod", "expected manifest with correct ID")
	}

	//
	// Disabling the daemon set and making a change should not do anything
	//
	mutator = func(dsToChange ds_fields.DaemonSet) (ds_fields.DaemonSet, error) {
		dsToChange.Disabled = true
		dsToChange.NodeSelector = klabels.Everything().
			Add("nodeQuality", klabels.EqualsOperator, []string{"good"})
		return dsToChange, nil
	}
	_, err = dsStore.MutateDS(ds.ID(), mutator)
	Assert(t).IsNil(err, "Unxpected error trying to mutate daemon set")

	numNodes = waitForNodes(t, ds, 1, desiresErrCh, changesErrCh)
	Assert(t).AreEqual(numNodes, 1, "took too long to unschedule")

	//
	// Now re-enable it and try to schedule everything
	//
	mutator = func(dsToChange ds_fields.DaemonSet) (ds_fields.DaemonSet, error) {
		dsToChange.NodeSelector = klabels.Everything()
		dsToChange.Disabled = false
		return dsToChange, nil
	}
	_, err = dsStore.MutateDS(ds.ID(), mutator)
	Assert(t).IsNil(err, "Unxpected error trying to mutate daemon set")

	// 11 good nodes 11 bad nodes, and 1 good cherry picked node = 23 nodes
	numNodes = waitForNodes(t, ds, 23, desiresErrCh, changesErrCh)
	Assert(t).AreEqual(numNodes, 23, "took too long to schedule")

	//
	// Deleting the daemon set should unschedule all of its nodes
	//
	ds.logger.NoFields().Info("Deleting daemon set...")
	err = dsStore.Delete(ds.ID())
	if err != nil {
		t.Fatalf("Unable to delete daemon set: %v", err)
	}
	numNodes = waitForNodes(t, ds, 0, desiresErrCh, changesErrCh)
	Assert(t).AreEqual(numNodes, 0, "took too long to unschedule")

	scheduled = scheduledPods(t, ds)
	Assert(t).AreEqual(len(scheduled), 0, "expected all nodes to have been unlabeled")
	manifestResults, _, err = kpStore.AllPods(kp.INTENT_TREE)
	if err != nil {
		t.Fatalf("Unable to get all pods from pod store: %v", err)
	}
	Assert(t).AreEqual(len(manifestResults), 0, "expected all manifests to have been unscheduled")
}
