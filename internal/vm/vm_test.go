package vm

import "testing"

func TestResourcesArithmeticClampsAtZero(t *testing.T) {
	total := Resources{VCPUs: 8, MemoryMiB: 4096, DiskGiB: 100}
	// Subtracting more than exists must not wrap into a negative allocation,
	// which would make an exhausted machine look like it had room.
	got := total.Sub(Resources{VCPUs: 32, MemoryMiB: 65536, DiskGiB: 1000})
	if got != (Resources{}) {
		t.Fatalf("Sub(oversized) = %s, want zero on every axis", got)
	}
	if sum := total.Add(Resources{VCPUs: 2, MemoryMiB: 1024, DiskGiB: 10}); sum.VCPUs != 10 || sum.MemoryMiB != 5120 || sum.DiskGiB != 110 {
		t.Fatalf("Add = %s", sum)
	}
}

func TestResourcesFitsRequiresEveryAxis(t *testing.T) {
	free := Resources{VCPUs: 4, MemoryMiB: 4096, DiskGiB: 50}
	if !free.Fits(Resources{VCPUs: 4, MemoryMiB: 4096, DiskGiB: 50}) {
		t.Error("an exact fit should be admitted")
	}
	// Short on a single axis is still a no.
	if free.Fits(Resources{VCPUs: 4, MemoryMiB: 4096, DiskGiB: 51}) {
		t.Error("a request exceeding disk should not fit")
	}
	if free.Fits(Resources{VCPUs: 5, MemoryMiB: 1, DiskGiB: 1}) {
		t.Error("a request exceeding vcpus should not fit")
	}
}

func TestCapacityCanFitHonoursInstanceCap(t *testing.T) {
	c := Capacity{
		Total:        Resources{VCPUs: 64, MemoryMiB: 65536, DiskGiB: 1000},
		Used:         Resources{VCPUs: 2, MemoryMiB: 2048, DiskGiB: 10},
		Instances:    3,
		MaxInstances: 3,
	}
	small := Resources{VCPUs: 1, MemoryMiB: 512, DiskGiB: 5}
	if c.CanFit(small) {
		t.Error("instance cap should refuse admission despite spare resources")
	}
	c.MaxInstances = 0
	if !c.CanFit(small) {
		t.Error("with no instance cap, spare resources should admit")
	}
}

func TestExecResultOK(t *testing.T) {
	if (&ExecResult{ExitCode: 0}).OK() != true {
		t.Error("a clean exit is OK")
	}
	if (&ExecResult{ExitCode: 1}).OK() {
		t.Error("a non-zero exit is not OK")
	}
	if (&ExecResult{ExitCode: 0, TimedOut: true}).OK() {
		t.Error("a timed-out command is not OK even with exit code 0")
	}
	var nilResult *ExecResult
	if nilResult.OK() {
		t.Error("a nil result is not OK")
	}
}

func TestSpecValidate(t *testing.T) {
	valid := Spec{Name: "n", Image: "i", Resources: Resources{VCPUs: 1, MemoryMiB: 512, DiskGiB: 5}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate(valid) = %v", err)
	}
	if err := (Spec{Image: "i", Resources: valid.Resources}).Validate(); err == nil {
		t.Error("a spec without a name should be rejected")
	}
}
