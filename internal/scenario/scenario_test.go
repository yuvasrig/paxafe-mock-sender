package scenario

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

var fixedNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func params() Params { return Params{Account: 478, Points: 12, Now: fixedNow, Seed: 42} }

func build(t *testing.T, id string) []Step {
	t.Helper()
	s, ok := Get(id)
	if !ok {
		t.Fatalf("scenario %q not registered", id)
	}
	return s.Build(params())
}

func TestAllScenariosRegistered(t *testing.T) {
	var ids []string
	for _, s := range All() {
		ids = append(ids, s.ID)
		if s.Title == "" || s.Description == "" {
			t.Errorf("%s needs a title and description", s.ID)
		}
		if (s.Build == nil) == (s.Next == nil) || s.Load != (s.Next != nil) {
			t.Errorf("%s must define Build (or Next for load) consistently", s.ID)
		}
	}
	want := "samples,route,excursion,fault,duplicates,malformed,auth,load"
	if strings.Join(ids, ",") != want {
		t.Fatalf("scenarios = %v, want %s", ids, want)
	}
}

func TestSamples(t *testing.T) {
	steps, err := Samples()
	if err != nil || len(steps) != 13 {
		t.Fatalf("%d steps, %v", len(steps), err)
	}
	for i, s := range steps {
		if !json.Valid(s.Body) || !s.Expect.Ack || !s.Expect.ExactFlags {
			t.Errorf("step %d: %+v", i, s.Expect)
		}
	}
	if e := steps[0].Expect; e.HTTP[0] != 200 || e.Status[0] != "flagged" || strings.Join(e.Flags, ",") != "IDENTITY_MISMATCH" {
		t.Errorf("sample 0 expectation = %+v", e)
	}
	if e := steps[2].Expect; len(e.Status) != 2 || e.Status[1] != "duplicate" {
		t.Errorf("accepted samples tolerate a re-run: %+v", e)
	}
	if e := steps[1].Expect; len(e.Status) != 1 || e.Status[0] != "conflict" {
		t.Errorf("conflict samples repeat on a re-run: %+v", e)
	}
	if e := steps[12].Expect; e.HTTP[0] != 422 || !e.Rejection || strings.Join(e.Flags, ",") != "IDENTITY_MISMATCH" {
		t.Errorf("sample 12 expectation = %+v", e)
	}
	if e := steps[9].Expect; len(e.Flags) != 0 || e.Status[0] != "accepted" {
		t.Errorf("sample 9 (AccountId 999) is a normal accepted sample: %+v", e)
	}
	if e := steps[11].Expect; len(e.Flags) != 0 || e.Status[0] != "accepted" {
		t.Errorf("sample 11 (EntryTimeUtc 5 h off) is accepted with no flags: %+v", e)
	}
	if e := steps[0].Expect; strings.Join(e.Flags, ",") != "IDENTITY_MISMATCH" {
		t.Errorf("sample 0 expectation = %+v", e)
	}
}

func tiveSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile("data/tive-incoming-schema.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("tive.json", doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("tive.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not JSON: %s", b)
	}
	return m
}

func TestGeneratedPayloadsMatchTiveSchema(t *testing.T) {
	schema := tiveSchema(t)
	// Scenarios whose payloads are valid per the Tive schema. Fault, malformed and excursion values
	// are chosen on purpose, but sentinels still match the schema; malformed is excluded.
	for _, id := range []string{"route", "excursion", "fault", "duplicates", "auth"} {
		for _, st := range build(t, id) {
			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(st.Body))
			if err != nil {
				t.Fatalf("%s %s: %v", id, st.Name, err)
			}
			if err := schema.Validate(inst); err != nil {
				t.Errorf("%s %q does not match the Tive schema: %v", id, st.Name, err)
			}
		}
	}
	s, _ := Get("load")
	for i := 0; i < 20; i++ {
		inst, _ := jsonschema.UnmarshalJSON(bytes.NewReader(s.Next(params(), i).Body))
		if err := schema.Validate(inst); err != nil {
			t.Errorf("load %d: %v", i, err)
		}
	}
}

func TestRoute(t *testing.T) {
	steps := build(t, "route")
	if len(steps) != 12 {
		t.Fatalf("%d steps", len(steps))
	}
	var prev int64
	imei := ""
	for i, st := range steps {
		m := decode(t, st.Body)
		ts := int64(m["EntryTimeEpoch"].(float64))
		if i > 0 && ts-prev != int64(routeSpacing/time.Millisecond) {
			t.Fatalf("step %d: readings must be 5 minutes apart, got %d ms", i, ts-prev)
		}
		prev = ts
		if i == len(steps)-1 && ts != fixedNow.UnixMilli() {
			t.Fatalf("the route ends at Now, got %d", ts)
		}
		temp := m["Temperature"].(map[string]any)["Celsius"].(float64)
		if temp < 2 || temp > 8 {
			t.Errorf("step %d: %v C is outside the 2-8 band", i, temp)
		}
		if imei == "" {
			imei = m["DeviceId"].(string)
		} else if m["DeviceId"] != imei {
			t.Fatal("one device for the whole route")
		}
		if m["AccountId"] != float64(478) {
			t.Fatalf("AccountId = %v", m["AccountId"])
		}
		if st.Expect.Status[0] != "accepted" || !st.Expect.Ack {
			t.Fatalf("expectation %+v", st.Expect)
		}
	}
	first, last := decode(t, steps[0].Body)["Location"].(map[string]any), decode(t, steps[11].Body)["Location"].(map[string]any)
	if first["Latitude"].(float64) < 42 || last["Latitude"].(float64) > 34 || last["Longitude"].(float64) > -84 {
		t.Fatalf("route should run Boston to Atlanta: %v -> %v", first, last)
	}
}

func TestDeviceIdentity(t *testing.T) {
	a, b := newDevice(1), newDevice(1)
	if a != b {
		t.Fatal("same seed must give the same device")
	}
	if c := newDevice(2); c.imei == a.imei {
		t.Fatal("different seeds should give different devices")
	}
	if len(a.imei) != 15 || !strings.HasPrefix(a.imei, "86") {
		t.Fatalf("imei %q", a.imei)
	}
}

func TestAccountOmittedWhenZero(t *testing.T) {
	s, _ := Get("route")
	p := params()
	p.Account = 0
	if _, has := decode(t, s.Build(p)[0].Body)["AccountId"]; has {
		t.Fatal("Account 0 must leave AccountId out")
	}
}

func TestExcursionReaches12(t *testing.T) {
	steps := build(t, "excursion")
	max, above := 0.0, 0
	for _, st := range steps {
		v := decode(t, st.Body)["Temperature"].(map[string]any)["Celsius"].(float64)
		if v > max {
			max = v
		}
		if v > 8 {
			above++
		}
		if len(st.Expect.Flags) != 0 || st.Expect.Status[0] != "accepted" {
			t.Fatalf("%.2f C: temperatures are not judged, so every reading is accepted with no flag: %+v", v, st.Expect)
		}
	}
	if max < 11.9 || above == 0 {
		t.Fatalf("max %.2f, readings above 8 C: %d", max, above)
	}
}

func TestFaultScenario(t *testing.T) {
	steps := build(t, "fault")
	faults, fallback := 0, 0
	for _, st := range steps {
		for _, f := range st.Expect.Flags {
			if f == "SENSOR_FAULT" {
				faults++
			}
			if f == "TEMPERATURE_FALLBACK" {
				fallback++
			}
		}
	}
	if faults < 3 || fallback != 1 {
		t.Fatalf("faults %d fallback %d", faults, fallback)
	}
}

func TestDuplicatesScenario(t *testing.T) {
	steps := build(t, "duplicates")
	conflicts := 0
	for i, st := range steps {
		switch {
		case strings.HasSuffix(st.Name, "repeat"):
			if !bytes.Equal(st.Body, steps[i-1].Body) || st.Expect.Status[0] != "duplicate" {
				t.Fatalf("%s must repeat the previous body and expect duplicate", st.Name)
			}
		case strings.HasSuffix(st.Name, "changed"):
			conflicts++
			if bytes.Equal(st.Body, steps[i-2].Body) || st.Expect.Status[0] != "conflict" {
				t.Fatalf("%s must differ from the first send and expect conflict", st.Name)
			}
		}
	}
	if conflicts != 2 {
		t.Fatalf("conflicts = %d", conflicts)
	}
}

func TestMalformedScenario(t *testing.T) {
	steps := build(t, "malformed")
	if len(steps) != 10 {
		t.Fatalf("%d steps", len(steps))
	}
	notJSON, bad400 := 0, 0
	for _, st := range steps {
		if !json.Valid(st.Body) {
			notJSON++
			if st.Expect.HTTP[0] != 400 {
				t.Errorf("%s: non-JSON must expect 400", st.Name)
			}
		}
		if st.Expect.HTTP[0] == 400 {
			bad400++
		}
		if !st.Expect.Ack {
			t.Errorf("%s: every malformed payload must still be acknowledged and archived", st.Name)
		}
	}
	if notJSON != 1 || bad400 != 2 { // garbage text, plus a JSON array that is not an object
		t.Fatalf("non-JSON bodies = %d, expecting-400 steps = %d", notJSON, bad400)
	}
}

func TestAuthScenario(t *testing.T) {
	steps := build(t, "auth")
	modes := map[KeyMode]bool{}
	for _, st := range steps {
		modes[st.Key] = true
		if st.Expect.HTTP[0] != 401 || st.Expect.Ack {
			t.Errorf("%s: %+v", st.Name, st.Expect)
		}
	}
	if len(modes) != 3 || modes[KeyDefault] {
		t.Fatalf("key modes %v", modes)
	}
}

// Load: no (device, timestamp) pair repeats, so nothing in a run is a duplicate.
func TestLoadPayloadsAreUnique(t *testing.T) {
	s, _ := Get("load")
	seen := map[string]bool{}
	for i := 0; i < 5000; i++ {
		m := decode(t, s.Next(params(), i).Body)
		k := m["DeviceId"].(string) + "|" + json.Number(strings.TrimSuffix(strings.TrimSpace(formatNum(m["EntryTimeEpoch"])), ".0")).String()
		if seen[k] {
			t.Fatalf("request %d repeats (device, timestamp) %s", i, k)
		}
		seen[k] = true
		if ts := int64(m["EntryTimeEpoch"].(float64)); ts > fixedNow.UnixMilli() {
			t.Fatalf("request %d is in the future", i)
		}
	}
}

func formatNum(v any) string { b, _ := json.Marshal(v); return string(b) }
