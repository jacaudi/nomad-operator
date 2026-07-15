package controller

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	nomadv1alpha1 "github.com/jacaudi/nomad-operator/api/v1alpha1"
)

func specWith(jobJSON string) nomadv1alpha1.NomadJobSpec {
	return nomadv1alpha1.NomadJobSpec{
		ClusterRef: nomadv1alpha1.JobClusterRef{Name: "prod"},
		JobID:      "web",
		Job:        runtime.RawExtension{Raw: []byte(jobJSON)},
	}
}

func TestDecodeJob_CamelAndPascalEquivalent(t *testing.T) {
	camel, err := decodeJob(specWith(`{"type":"service","taskGroups":[{"name":"app","count":3,"tasks":[{"name":"s","driver":"docker","resources":{"cpu":200,"memoryMB":128}}]}]}`), "global")
	if err != nil {
		t.Fatalf("camel decode: %v", err)
	}
	pascal, err := decodeJob(specWith(`{"Type":"service","TaskGroups":[{"Name":"app","Count":3,"Tasks":[{"Name":"s","Driver":"docker","Resources":{"CPU":200,"MemoryMB":128}}]}]}`), "global")
	if err != nil {
		t.Fatalf("pascal decode: %v", err)
	}
	if len(camel.TaskGroups) != 1 || camel.TaskGroups[0].Count == nil || *camel.TaskGroups[0].Count != 3 {
		t.Fatalf("camel not parsed: %+v", camel)
	}
	if len(pascal.TaskGroups) != 1 || pascal.TaskGroups[0].Count == nil || *pascal.TaskGroups[0].Count != 3 {
		t.Fatalf("pascal not parsed: %+v", pascal)
	}
	if pascal.TaskGroups[0].Tasks[0].Resources.MemoryMB == nil || *pascal.TaskGroups[0].Tasks[0].Resources.MemoryMB != 128 {
		t.Fatalf("nested resources not parsed")
	}
}

func TestDecodeJob_InjectsIDAndRegion(t *testing.T) {
	job, err := decodeJob(specWith(`{"type":"service"}`), "us-east")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if job.ID == nil || *job.ID != "web" {
		t.Fatalf("job.ID = %v, want web", job.ID)
	}
	if job.Region == nil || *job.Region != "us-east" {
		t.Fatalf("job.Region = %v, want us-east", job.Region)
	}
}

func TestDecodeJob_RejectsUnknownKey(t *testing.T) {
	_, err := decodeJob(specWith(`{"type":"service","taskGropus":[]}`), "global")
	if err == nil {
		t.Fatal("unknown key must be rejected (strict decode)")
	}
	if errors.Is(err, errJobIDMismatch) {
		t.Fatal("unknown-key error must not be a jobID mismatch")
	}
}

func TestDecodeJob_RejectsWrongScalarType(t *testing.T) {
	if _, err := decodeJob(specWith(`{"taskGroups":[{"name":"app","count":"three"}]}`), "global"); err == nil {
		t.Fatal("count as string must be rejected")
	}
}

func TestDecodeJob_DurationMustBeNanoseconds(t *testing.T) {
	// HCL-style "10s" is rejected; integer nanoseconds decode (SGE I-1).
	if _, err := decodeJob(specWith(`{"update":{"minHealthyTime":"10s"}}`), "global"); err == nil {
		t.Fatal(`update.minHealthyTime:"10s" must be rejected (time.Duration wants int ns)`)
	}
	if _, err := decodeJob(specWith(`{"update":{"minHealthyTime":10000000000}}`), "global"); err != nil {
		t.Fatalf("minHealthyTime in ns must decode: %v", err)
	}
}

func TestDecodeJob_JobIDMismatch(t *testing.T) {
	_, err := decodeJob(specWith(`{"id":"other","type":"service"}`), "global")
	if err == nil || !errors.Is(err, errJobIDMismatch) {
		t.Fatalf("mismatched blob id must wrap errJobIDMismatch, got %v", err)
	}
}
