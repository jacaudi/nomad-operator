package nomad

import (
	"net/http"
	"testing"

	"github.com/hashicorp/nomad/api"
)

// newTestClient(t, h) lives in nodepool_test.go (same package) — reuse it.

func TestGetJob_NotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "job not found", http.StatusNotFound)
	})
	job, err := c.GetJob(t.Context(), "missing")
	if err != nil || job != nil {
		t.Fatalf("GetJob 404 = (%v, %v), want (nil, nil)", job, err)
	}
}

func TestPlanJob_ChangedAndNone(t *testing.T) {
	diffType := "Added"
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Diff":{"Type":"` + diffType + `"}}`))
	})
	id := "web"
	changed, err := c.PlanJob(t.Context(), &api.Job{ID: &id})
	if err != nil || !changed {
		t.Fatalf("PlanJob Added = (%v, %v), want (true, nil)", changed, err)
	}
	diffType = "None"
	changed, err = c.PlanJob(t.Context(), &api.Job{ID: &id})
	if err != nil || changed {
		t.Fatalf("PlanJob None = (%v, %v), want (false, nil)", changed, err)
	}
}

func TestPlanJob_NilDiffTreatedAsChanged(t *testing.T) {
	// A Plan response with no Diff field (Nomad returns no computed diff) must
	// take PlanJob's documented safe fallback: treat as changed so the job is
	// still Registered (at worst one extra upsert), never silently skipped.
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	id := "web"
	changed, err := c.PlanJob(t.Context(), &api.Job{ID: &id})
	if err != nil || !changed {
		t.Fatalf("PlanJob nil-Diff = (%v, %v), want (true, nil)", changed, err)
	}
}

func TestRegisterJob_Warnings(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"EvalID":"e1","Warnings":"deprecated foo"}`))
	})
	id := "web"
	warn, err := c.RegisterJob(t.Context(), &api.Job{ID: &id})
	if err != nil || warn != "deprecated foo" {
		t.Fatalf("RegisterJob = (%q, %v), want (\"deprecated foo\", nil)", warn, err)
	}
}

func TestJobGroupSummary(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"JobID":"web","Summary":{"app":{"Running":2,"Starting":1}}}`))
	})
	got, err := c.JobGroupSummary(t.Context(), "web")
	if err != nil {
		t.Fatalf("JobGroupSummary: %v", err)
	}
	if got["app"].Running != 2 || got["app"].Starting != 1 {
		t.Fatalf("summary = %+v, want app{Running:2,Starting:1}", got)
	}
}

func TestDeregisterJob_OK(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"EvalID":"e1"}`))
	})
	if err := c.DeregisterJob(t.Context(), "web", true); err != nil {
		t.Fatalf("DeregisterJob: %v", err)
	}
}
