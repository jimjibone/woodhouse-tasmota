package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimjibone/wh/v2/bridges"
	"github.com/jimjibone/wh/v2/bridges/services"
	clientsapi "github.com/jimjibone/woodhouse-api/go/v2/clients"
	"github.com/jimjibone/woodhouse-api/values"
)

const testMAC = "aabbccddeeff"

// Fictional Status 0 responses. StatusSTS has the shape of a STATE message and
// StatusSNS the shape of a SENSOR message.

const singleRelayStatus = `{
  "Status": {"Module": 1, "DeviceName": "Example Plug", "FriendlyName": ["Example Plug"], "Topic": "example_plug", "Power": 1},
  "StatusNET": {"Hostname": "example-plug", "IPAddress": "192.0.2.10", "Mac": "AA:BB:CC:DD:EE:FF"},
  "StatusSTS": {"Time": "2026-01-01T00:00:00", "Uptime": "1T00:00:00", "POWER": "ON", "Wifi": {"RSSI": 60}},
  "StatusSNS": {"Time": "2026-01-01T00:00:00", "ENERGY": {
    "TotalStartTime": "2025-06-01T00:00:00", "Total": 12.345, "Yesterday": 0.5, "Today": 0.25,
    "Power": 42, "ApparentPower": 50, "ReactivePower": 20, "Factor": 0.84, "Voltage": 231, "Current": 0.182}}
}`

const twoRelayStatus = `{
  "Status": {"Module": 8, "DeviceName": "Example Switch", "FriendlyName": ["Example Switch", "Kitchen Light"], "Topic": "example_switch"},
  "StatusNET": {"Mac": "AA:BB:CC:DD:EE:FF"},
  "StatusSTS": {"POWER1": "ON", "POWER2": "OFF"},
  "StatusSNS": {"Time": "2026-01-01T00:00:00"}
}`

const dualMeterStatus = `{
  "Status": {"FriendlyName": ["Dual", ""], "Topic": "example_dual"},
  "StatusNET": {"Mac": "AA:BB:CC:DD:EE:FF"},
  "StatusSTS": {"POWER1": "OFF", "POWER2": "ON"},
  "StatusSNS": {"TempUnit": "F", "ESP32": {"Temperature": 113.0},
    "ENERGY": {"Total": [1.5, 2.5], "Power": [10, 20], "Voltage": 230, "Current": [0.05, 0.1]}}
}`

const wholeDeviceMeterStatus = `{
  "Status": {"FriendlyName": ["Example Switch", "Second"], "Topic": "example_switch"},
  "StatusNET": {"Mac": "AA:BB:CC:DD:EE:FF"},
  "StatusSTS": {"POWER1": "OFF", "POWER2": "OFF"},
  "StatusSNS": {"ENERGY": {"Power": 100, "Total": 7}}
}`

func parseStatus(t *testing.T, raw string) *StatusResponse {
	t.Helper()
	status := &StatusResponse{}
	if err := json.Unmarshal([]byte(raw), status); err != nil {
		t.Fatal(err)
	}
	return status
}

// testStates maps a published *bridges.Device to a channel of the states it
// sends.
var testStates sync.Map

type fakeTasmota struct {
	mu       sync.Mutex
	commands []string
	response string // body to answer commands with
	err      error
}

func (f *fakeTasmota) command(_ context.Context, command string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	return []byte(f.response), f.err
}

// newTestDevice applies the status to a new device and returns it and the
// bridge device it published.
func newTestDevice(t *testing.T, status string, fake *fakeTasmota) (*TasmotaDevice, *bridges.Device) {
	t.Helper()
	var published []*bridges.Device
	dev, err := newTasmotaDevice(func(d *bridges.Device) error {
		// Register for sent states as the bridge does when adding a device.
		states := make(chan *clientsapi.Device, 256)
		d.Init(func(state *clientsapi.Device) {
			select {
			case states <- state:
			default:
			}
		})
		testStates.Store(d, states)
		published = append(published, d)
		return nil
	}, testMAC, "192.0.2.10", fake.command)
	if err != nil {
		t.Fatal(err)
	}
	if len(published) != 0 {
		t.Fatal("device published before first status")
	}
	dev.applyStatus(parseStatus(t, status))
	if len(published) != 1 {
		t.Fatalf("published %d devices, want 1", len(published))
	}
	t.Cleanup(published[0].Close)
	return dev, published[0]
}

// shape returns the device's endpoints as a map of endpoint ID to
// "label: service ids", in the order sent.
func shape(t *testing.T, d *bridges.Device) (ids []string, byID map[string]string) {
	t.Helper()
	val, _ := testStates.Load(d)
	got := val.(chan *clientsapi.Device)
	d.SendFullState()
	var state *clientsapi.Device
	for state == nil {
		select {
		case s := <-got:
			if s.FullState {
				state = s
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no full state")
		}
	}
	byID = make(map[string]string)
	for _, ep := range state.Endpoints {
		var srvs []string
		for _, srv := range ep.Services {
			srvs = append(srvs, srv.Id)
		}
		slices.Sort(srvs)
		ids = append(ids, ep.Id)
		byID[ep.Id] = ep.Label + ": " + strings.Join(srvs, ",")
	}
	return ids, byID
}

func service[T services.Service](t *testing.T, d *bridges.Device, endpoint string) T {
	t.Helper()
	srv, ok := d.Endpoint(endpoint).Service(snake[T]()).(T)
	if !ok {
		t.Fatalf("endpoint %q has no %s", endpoint, snake[T]())
	}
	return srv
}

func snake[T services.Service]() string {
	var zero T
	switch any(zero).(type) {
	case *services.OnOff:
		return "on_off"
	case *services.PowerMeter:
		return "power_meter"
	case *services.DeviceTemperature:
		return "device_temperature"
	}
	panic("unknown service type")
}

func assertShape(t *testing.T, d *bridges.Device, want map[string]string, order []string) {
	t.Helper()
	ids, got := shape(t, d)
	if !slices.Equal(ids, order) {
		t.Fatalf("endpoints %v, want %v", ids, order)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("endpoint %q = %q, want %q", id, got[id], w)
		}
	}
}

func approx(a, b float64) bool {
	d := a - b
	return d < 0.0001 && d > -0.0001
}

func TestSingleRelayWithEnergy(t *testing.T) {
	dev, d := newTestDevice(t, singleRelayStatus, &fakeTasmota{})

	if d.ID() != "tasmotaaabbccddeeff" {
		t.Fatalf("device id %q", d.ID())
	}
	assertShape(t, d, map[string]string{
		"0": ": info,online",
		"1": "Example Plug: on_off,power_meter",
	}, []string{"0", "1"})

	if !service[*services.OnOff](t, d, "1").On.Get() {
		t.Error("channel 1 should be on")
	}
	meter := service[*services.PowerMeter](t, d, "1")
	for name, c := range map[string]struct{ got, want float64 }{
		"power":   {meter.Power.Get(), 42},
		"voltage": {meter.Voltage.Get(), 231},
		"current": {meter.Current.Get(), 0.182},
		"energy":  {meter.Energy.Get(), 12.345},
	} {
		if !approx(c.got, c.want) {
			t.Errorf("%s = %v, want %v", name, c.got, c.want)
		}
	}
	if got := d.Info.Name.Get(); got != "Example Plug" {
		t.Errorf("name %q", got)
	}
	if !d.Online.Online.Get() {
		t.Error("should be online")
	}
	if dev.temp != nil {
		t.Error("no device temperature expected")
	}

	// The device must not offer update or identify: neither is implemented.
	if d.Root().Service("update") != nil || d.Root().Service("identify") != nil {
		t.Error("update/identify should be omitted")
	}
}

func TestSingleRelayEnergyReportsOnlyWhatDeviceHas(t *testing.T) {
	status := strings.Replace(singleRelayStatus, `"Voltage": 231, "Current": 0.182`, `"Today": 1`, 1)
	_, d := newTestDevice(t, status, &fakeTasmota{})
	meter := service[*services.PowerMeter](t, d, "1")
	if meter.Voltage.IsSet() || meter.Current.IsSet() {
		t.Error("voltage and current were not reported")
	}
	if !meter.Power.IsSet() || !meter.Energy.IsSet() {
		t.Error("power and energy were reported")
	}
	for _, attr := range meter.Pb().Attrs {
		if attr.Id == "voltage" || attr.Id == "current" {
			t.Errorf("unreported attribute %q sent", attr.Id)
		}
	}
}

func TestSingleRelayWithoutLabelOrEnergy(t *testing.T) {
	status := `{"Status": {"FriendlyName": [], "Topic": "example_plug"},
		"StatusNET": {"Mac": "AA:BB:CC:DD:EE:FF"}, "StatusSTS": {"POWER": "OFF"}}`
	_, d := newTestDevice(t, status, &fakeTasmota{})
	assertShape(t, d, map[string]string{
		"1": "Output 1: on_off",
	}, []string{"0", "1"})
	if d.Info.Name.Get() != "example_plug" {
		t.Errorf("name should fall back to topic, got %q", d.Info.Name.Get())
	}
	if service[*services.OnOff](t, d, "1").On.Get() {
		t.Error("should be off")
	}
}

func TestMultiPowerLabelsAndState(t *testing.T) {
	_, d := newTestDevice(t, twoRelayStatus, &fakeTasmota{})
	assertShape(t, d, map[string]string{
		"0": ": info,online",
		"1": "Example Switch: on_off",
		"2": "Kitchen Light: on_off",
	}, []string{"0", "1", "2"})
	if !service[*services.OnOff](t, d, "1").On.Get() || service[*services.OnOff](t, d, "2").On.Get() {
		t.Error("expected channel 1 on, channel 2 off")
	}
}

func TestMultipleMetersMapToChannelsAndDeviceTemperature(t *testing.T) {
	dev, d := newTestDevice(t, dualMeterStatus, &fakeTasmota{})
	assertShape(t, d, map[string]string{
		"0": ": device_temperature,info,online",
		"1": "Dual: on_off,power_meter",
		"2": "Output 2: on_off,power_meter",
	}, []string{"0", "1", "2"})

	m1 := service[*services.PowerMeter](t, d, "1")
	m2 := service[*services.PowerMeter](t, d, "2")
	if !approx(m1.Power.Get(), 10) || !approx(m2.Power.Get(), 20) {
		t.Errorf("power %v, %v", m1.Power.Get(), m2.Power.Get())
	}
	if !approx(m1.Energy.Get(), 1.5) || !approx(m2.Energy.Get(), 2.5) {
		t.Errorf("energy %v, %v", m1.Energy.Get(), m2.Energy.Get())
	}
	// A plain number alongside arrays applies to every meter.
	if !approx(m1.Voltage.Get(), 230) || !approx(m2.Voltage.Get(), 230) {
		t.Errorf("voltage %v, %v", m1.Voltage.Get(), m2.Voltage.Get())
	}
	if !approx(m1.Current.Get(), 0.05) || !approx(m2.Current.Get(), 0.1) {
		t.Errorf("current %v, %v", m1.Current.Get(), m2.Current.Get())
	}
	// 113 F is 45 C.
	if got := dev.temp.Temperature.Get(); !approx(got, 45) {
		t.Errorf("device temperature %v, want 45", got)
	}
}

func TestSingleMeterOnMultiChannelDeviceGoesOnRoot(t *testing.T) {
	_, d := newTestDevice(t, wholeDeviceMeterStatus, &fakeTasmota{})
	assertShape(t, d, map[string]string{
		"0": ": info,online,power_meter",
		"1": "Example Switch: on_off",
		"2": "Second: on_off",
	}, []string{"0", "1", "2"})
	meter := service[*services.PowerMeter](t, d, "0")
	if !approx(meter.Power.Get(), 100) || !approx(meter.Energy.Get(), 7) {
		t.Errorf("power %v, energy %v", meter.Power.Get(), meter.Energy.Get())
	}
}

func TestStateAndSensorMessagesUpdateValues(t *testing.T) {
	dev, d := newTestDevice(t, singleRelayStatus, &fakeTasmota{})
	out := service[*services.OnOff](t, d, "1")
	meter := service[*services.PowerMeter](t, d, "1")

	// A STATE message, as published over MQTT.
	power, err := ParsePower([]byte(`{"Time":"2026-01-01T00:00:05","Uptime":"1T00:00:05","POWER":"OFF","Heap":24}`))
	if err != nil {
		t.Fatal(err)
	}
	dev.applyPower(power)
	if out.On.Get() {
		t.Error("should be off after STATE")
	}

	// A SENSOR message.
	sensor, err := ParseSensor([]byte(`{"Time":"2026-01-01T00:00:05","ENERGY":{"Total":12.4,"Power":0,"Voltage":232,"Current":0}}`))
	if err != nil {
		t.Fatal(err)
	}
	dev.applySensor(sensor)
	if !approx(meter.Power.Get(), 0) || !approx(meter.Voltage.Get(), 232) || !approx(meter.Energy.Get(), 12.4) {
		t.Errorf("power %v voltage %v energy %v", meter.Power.Get(), meter.Voltage.Get(), meter.Energy.Get())
	}
}

func TestOfflineAndOnline(t *testing.T) {
	dev, d := newTestDevice(t, twoRelayStatus, &fakeTasmota{})
	if !d.Online.Online.Get() {
		t.Fatal("should be online")
	}
	// PollStatus cannot reach 192.0.2.10 within the test; use a cancelled
	// context so it fails at once.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dev.PollStatus(ctx)
	if d.Online.Online.Get() {
		t.Error("should be offline after a failed poll")
	}
	dev.applyStatus(parseStatus(t, twoRelayStatus))
	if !d.Online.Online.Get() {
		t.Error("should be online again")
	}
}

func TestOfflineBeforeFirstStatusPublishesNothing(t *testing.T) {
	published := 0
	dev, err := newTasmotaDevice(func(*bridges.Device) error { published++; return nil }, testMAC, "192.0.2.10", (&fakeTasmota{}).command)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dev.PollStatus(ctx)
	if published != 0 {
		t.Fatal("device published without a status")
	}
}

func actionRequest(endpoint string, action any) *clientsapi.ActionRequest {
	req := &clientsapi.ActionRequest{EndpointId: endpoint, ServiceId: "on_off"}
	switch a := action.(type) {
	case *clientsapi.ActionRequest_Write:
		req.Action = a
	case *clientsapi.ActionRequest_Command:
		req.Action = a
	}
	return req
}

func TestWriteSendsPowerCommandForChannel(t *testing.T) {
	fake := &fakeTasmota{response: `{"POWER2":"ON"}`}
	_, d := newTestDevice(t, twoRelayStatus, fake)

	_, err := d.HandleAction(actionRequest("2", &clientsapi.ActionRequest_Write{Write: &clientsapi.WriteAction{
		Values: []*clientsapi.FieldValue{values.Field("on", values.Bool(true))},
	}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fake.commands, []string{"Power2%20on"}) {
		t.Fatalf("commands %v", fake.commands)
	}
	if !service[*services.OnOff](t, d, "2").On.Get() {
		t.Error("channel 2 should be on from the response")
	}
	if !service[*services.OnOff](t, d, "1").On.Get() {
		t.Error("channel 1 should be untouched")
	}
}

func TestToggleUsesTasmotaToggle(t *testing.T) {
	fake := &fakeTasmota{response: `{"POWER":"OFF"}`}
	_, d := newTestDevice(t, singleRelayStatus, fake)

	_, err := d.HandleAction(actionRequest("1", &clientsapi.ActionRequest_Command{Command: &clientsapi.CommandAction{Id: "toggle"}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fake.commands, []string{"Power1%20toggle"}) {
		t.Fatalf("commands %v", fake.commands)
	}
	if service[*services.OnOff](t, d, "1").On.Get() {
		t.Error("should be off from the response")
	}
}

func TestPowerCommandErrors(t *testing.T) {
	fake := &fakeTasmota{err: errors.New("timeout")}
	_, d := newTestDevice(t, singleRelayStatus, fake)
	toggle := actionRequest("1", &clientsapi.ActionRequest_Command{Command: &clientsapi.CommandAction{Id: "toggle"}})

	if _, err := d.HandleAction(toggle, nil); err == nil {
		t.Error("expected error when the request fails")
	}
	fake.err, fake.response = nil, `{"Command":"Unknown"}`
	if _, err := d.HandleAction(toggle, nil); err == nil {
		t.Error("expected error when the response has no power state")
	}
	if !service[*services.OnOff](t, d, "1").On.Get() {
		t.Error("state should not change on failure")
	}
}

func TestDeviceIDFromMAC(t *testing.T) {
	status := parseStatus(t, singleRelayStatus)
	if status.ID() != testMAC {
		t.Fatalf("status id %q", status.ID())
	}
	if _, err := newTasmotaDevice(nil, "", "192.0.2.10", nil); err == nil {
		t.Error("empty MAC should be rejected")
	}
}

func TestParsePower(t *testing.T) {
	got, err := ParsePower([]byte(`{"POWER1":"on","POWER3":"OFF","POWERX":"ON","Power":"ON","Heap":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[1] || got[3] {
		t.Errorf("got %v", got)
	}
	if _, err := ParsePower([]byte(`{"POWER":1}`)); err == nil {
		t.Error("non-string power state should be an error")
	}
	if got, err := ParsePower(nil); err != nil || len(got) != 0 {
		t.Errorf("empty payload: %v, %v", got, err)
	}
}

func TestParseSensorIgnoresAmbientTemperature(t *testing.T) {
	sensor, err := ParseSensor([]byte(`{"DS18B20":{"Id":"0123","Temperature":21.5},"TempUnit":"C"}`))
	if err != nil {
		t.Fatal(err)
	}
	if sensor.DeviceTemp != nil || len(sensor.Meters) != 0 {
		t.Errorf("got %+v", sensor)
	}
	sensor, err = ParseSensor([]byte(`{"ESP32":{"Temperature":48.3},"TempUnit":"C"}`))
	if err != nil {
		t.Fatal(err)
	}
	if sensor.DeviceTemp == nil || !approx(*sensor.DeviceTemp, 48.3) {
		t.Errorf("got %+v", sensor)
	}
}
