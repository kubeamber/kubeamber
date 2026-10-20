package changerequest

import (
	"context"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

const fp = "3fa9c2d1aa0e5b7c9d2f4e6a8b0c1d3e5f7a9b1c3d5e7f9a1b3c5d7e9f1a3b5c"

func newStore(t *testing.T) (*Store, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{GVR: "ChangeRequestList"})
	// The fake tracker does not implement generateName.
	n := 0
	client.PrependReactor("create", "changerequests", func(a k8stesting.Action) (bool, runtime.Object, error) {
		u := a.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		if u.GetName() == "" {
			n++
			u.SetName(fmt.Sprintf("%s%05d", u.GetGenerateName(), n))
			u.SetCreationTimestamp(metav1.NewTime(time.Unix(int64(n), 0)))
		}
		return false, nil, nil
	})
	return &Store{Client: client}, client
}

func TestCreateFindDecide(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)

	cr, err := s.Create(ctx, &ChangeRequest{Spec: Spec{
		Agent: "bot", User: "system:serviceaccount:agents:bot", Operation: "UPDATE", Score: 75,
		Target:      Target{Group: "apps", Version: "v1", Resource: "deployments", Kind: "Deployment", Namespace: "prod", Name: "checkout"},
		Fingerprint: fp, ExpiresAt: metav1.NewTime(now.Add(time.Hour)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cr.Name != "cr-3fa9c2d1-00001" || cr.Status.Phase != Pending {
		t.Fatalf("created %s in phase %s", cr.Name, cr.Status.Phase)
	}
	if _, err := s.Create(ctx, &ChangeRequest{Spec: Spec{Agent: "bot", Fingerprint: fp[:63] + "0"}}); err != nil {
		t.Fatal(err)
	}

	found, err := s.FindByFingerprint(ctx, fp)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Name != cr.Name || found[0].Spec.Target.Name != "checkout" {
		t.Fatalf("FindByFingerprint returned %d results (the label prefix must not be enough)", len(found))
	}

	if err := s.Decide(ctx, cr.Name, DecisionApproved, "alice", "go", now); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, cr.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != Approved || got.Spec.Approval.By != "alice" || got.Spec.Agent != "bot" {
		t.Fatalf("after Decide: phase %s approval %+v", got.Status.Phase, got.Spec.Approval)
	}
	if err := s.SetPhase(ctx, got, Consumed, now); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, cr.Name)
	if got.Status.Phase != Consumed || got.Status.ConsumedAt == nil {
		t.Errorf("after SetPhase: %+v", got.Status)
	}

	all, err := s.List(ctx)
	if err != nil || len(all) != 2 || all[0].Name != "cr-3fa9c2d1-00002" {
		t.Errorf("List: %d items, err %v", len(all), err)
	}
}

func TestSetPhaseConflict(t *testing.T) {
	s, client := newStore(t)
	ctx := context.Background()
	cr, err := s.Create(ctx, &ChangeRequest{Spec: Spec{Agent: "bot", Fingerprint: fp}})
	if err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("update", "changerequests", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(GVR.GroupResource(), cr.Name, fmt.Errorf("modified"))
	})
	if err := s.SetPhase(ctx, cr, Consumed, time.Now()); err != ErrAlreadyUsed {
		t.Errorf("SetPhase on conflict = %v, want ErrAlreadyUsed", err)
	}
}

func TestEffective(t *testing.T) {
	now := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	ttl := 15 * time.Minute
	approved := &ChangeRequest{Spec: Spec{ExpiresAt: metav1.NewTime(now.Add(time.Hour)),
		Approval: &Approval{Decision: DecisionApproved, At: metav1.NewTime(now)}}, Status: Status{Phase: Approved}}
	pending := &ChangeRequest{Spec: Spec{ExpiresAt: metav1.NewTime(now.Add(time.Hour))}, Status: Status{Phase: Pending}}
	cases := []struct {
		cr   *ChangeRequest
		at   time.Time
		want Phase
	}{
		{pending, now, Pending},
		{pending, now.Add(2 * time.Hour), Expired},
		{approved, now.Add(10 * time.Minute), Approved},
		{approved, now.Add(20 * time.Minute), Expired},
		{&ChangeRequest{Status: Status{Phase: Consumed}}, now, Consumed},
	}
	for i, c := range cases {
		if got := c.cr.Effective(c.at, ttl); got != c.want {
			t.Errorf("case %d: %s, want %s", i, got, c.want)
		}
	}
}
