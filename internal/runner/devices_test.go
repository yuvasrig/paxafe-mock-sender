package runner

import (
	"context"
	"testing"
	"time"

	"github.com/yuvasrig/paxafe-mock-sender/internal/api"
	"github.com/yuvasrig/paxafe-mock-sender/internal/scenario"
)

func TestDeviceOfRequest(t *testing.T) {
	imei, name := deviceOfRequest([]byte(`{"DeviceId":"863257063350583","DeviceName":"A571992","x":1}`))
	if imei != "863257063350583" || name != "A571992" {
		t.Fatalf("%q %q", imei, name)
	}
	if imei, name := deviceOfRequest([]byte(`{"DeviceName":"NOT-A-KNOWN-DEVICE"}`)); imei != "" || name != "NOT-A-KNOWN-DEVICE" {
		t.Fatalf("a payload with no DeviceId: %q %q", imei, name)
	}
	for _, bad := range []string{`not json`, `[1,2]`, `{"DeviceId":5}`, ``} {
		if imei, name := deviceOfRequest([]byte(bad)); imei != "" || name != "" {
			t.Errorf("%q gives %q %q", bad, imei, name)
		}
	}
}

func TestDevicesOfCountsPerDevicePerLoad(t *testing.T) {
	results := []Result{
		{Load: 1, DeviceIMEI: "861", DeviceName: "SIM1", EventID: "e1"}, {Load: 1, DeviceIMEI: "861", DeviceName: "SIM1", EventID: "e2"},
		{Load: 1, DeviceIMEI: "862", DeviceName: "SIM2"}, // never acknowledged
		{Load: 2, DeviceIMEI: "861", DeviceName: "SIM1", EventID: "e4"},
		{Load: 2, DeviceName: "NOT-A-KNOWN-DEVICE", EventID: "e5"},
		{}, // not a JSON object: no device to list
	}
	got := devicesOf(results, []LoadSummary{{Index: 1, Target: "Acme"}, {Index: 2, Target: "BioCo"}})
	want := []DeviceUse{
		{IMEI: "861", Name: "SIM1", Load: 1, Target: "Acme", Requests: 2, Acknowledged: 2},
		{IMEI: "862", Name: "SIM2", Load: 1, Target: "Acme", Requests: 1, Acknowledged: 0},
		{IMEI: "861", Name: "SIM1", Load: 2, Target: "BioCo", Requests: 1, Acknowledged: 1},
		{IMEI: "", Name: "NOT-A-KNOWN-DEVICE", Load: 2, Target: "BioCo", Requests: 1, Acknowledged: 1}, // no DeviceId: last
	}
	if len(got) != len(want) {
		t.Fatalf("%d devices: %+v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}

func TestARunListsItsDevices(t *testing.T) {
	f := newFake(t)
	cfg := f.cfg()
	cfg.Params.Points = 6
	rep, _ := Run(context.Background(), cfg, mustGet(t, "route"))
	if len(rep.Devices) != 1 || rep.Devices[0].Requests != 6 || rep.Devices[0].Acknowledged != 6 || len(rep.Devices[0].IMEI) != 15 || rep.Devices[0].Name == "" {
		t.Fatalf("the route is one device that sent 6 requests: %+v", rep.Devices)
	}
	// The same seed gives the same device: the list is repeatable.
	rep2, _ := Run(context.Background(), cfg, mustGet(t, "route"))
	if rep2.Devices[0].IMEI != rep.Devices[0].IMEI {
		t.Fatal("a fixed seed gives the same device")
	}
	cfg.Params.Seed++
	if rep3, _ := Run(context.Background(), cfg, mustGet(t, "route")); rep3.Devices[0].IMEI == rep.Devices[0].IMEI {
		t.Fatal("another seed gives another device")
	}
}

func TestAMalformedRunListsTheDeviceWithNoId(t *testing.T) {
	f := newFake(t)
	rep, _ := Run(context.Background(), f.cfg(), mustGet(t, "malformed"))
	var none bool
	for _, d := range rep.Devices {
		none = none || (d.IMEI == "" && d.Name == "NOT-A-KNOWN-DEVICE")
	}
	if !none {
		t.Fatalf("the payload that sends no DeviceId is listed as such: %+v", rep.Devices)
	}
}

func TestALoadPlanListsDevicesPerLoadAndTenant(t *testing.T) {
	keyA, keyB := "pxk_test_AAAAsecretA0000", "pxk_test_BBBBsecretB1111"
	f := newTenantAPI(t, keyA, keyB)
	rep, err := RunPlan(context.Background(), Config{Client: api.New(f.URL), RunID: "dev", Params: scenario.Params{Account: 1, Points: 6, Seed: 9}}, []Load{
		{Key: keyA, Label: "Acme", Scenarios: []string{"load"}, Rate: 100, Duration: time.Second},
		{Key: keyB, Label: "BioCo", Scenarios: []string{"route"}, Rate: 10, Duration: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	perLoad := map[int]int{}
	requests := map[int]int{}
	for _, d := range rep.Devices {
		perLoad[d.Load]++
		requests[d.Load] += d.Requests
		if d.Target == "" || d.IMEI == "" || len(d.IMEI) != 15 {
			t.Fatalf("every device carries its tenant and a valid IMEI: %+v", d)
		}
	}
	if perLoad[1] != 50 || requests[1] != 100 {
		t.Fatalf("the load scenario uses 50 simulated trackers: %d devices, %d requests", perLoad[1], requests[1])
	}
	// A scripted scenario inside a load starts a new tracker on each pass through it (a new seed, so a long load does not
	// repeat one device): 10 requests over a 6-reading route are two passes, so two devices.
	if perLoad[2] != 2 || requests[2] != 10 {
		t.Fatalf("a 6-reading route sending 10 requests is 2 devices: %d devices, %d requests", perLoad[2], requests[2])
	}
	if rep.Devices[0].Load != 1 || rep.Devices[len(rep.Devices)-1].Load != 2 {
		t.Fatal("listed load by load")
	}
}
