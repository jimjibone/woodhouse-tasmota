package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jimjibone/log"
	"github.com/jimjibone/wh/v2/bridges"
	"github.com/jimjibone/wh/v2/bridges/services"
	clientsapi "github.com/jimjibone/woodhouse-api/go/v2/clients"
)

// ParsePower returns the state of each power channel in a Tasmota state
// payload (the StatusSTS of a Status 0 response, an MQTT STATE message or the
// response to a Power command), keyed by channel number starting at 1. A
// single-relay device reports "POWER", which is channel 1; others report
// "POWER1", "POWER2", and so on.
func ParsePower(raw []byte) (map[int]bool, error) {
	power := make(map[int]bool)
	if len(raw) == 0 {
		return power, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for key, val := range fields {
		ch, ok := powerChannel(key)
		if !ok {
			continue
		}
		var state string
		if err := json.Unmarshal(val, &state); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		power[ch] = strings.EqualFold(state, "ON")
	}
	return power, nil
}

// powerChannel returns the channel number for a POWER, POWER1, ... key.
func powerChannel(key string) (int, bool) {
	suffix, ok := strings.CutPrefix(key, "POWER")
	if !ok {
		return 0, false
	}
	if suffix == "" {
		return 1, true
	}
	ch, err := strconv.Atoi(suffix)
	if err != nil || ch < 1 {
		return 0, false
	}
	return ch, true
}

func sortedChannels(power map[int]bool) []int {
	channels := make([]int, 0, len(power))
	for ch := range power {
		channels = append(channels, ch)
	}
	slices.Sort(channels)
	return channels
}

// MeterReading is one energy meter's readings from a Tasmota ENERGY object.
// Fields the device does not report are nil.
type MeterReading struct {
	Voltage *float64 // V
	Current *float64 // A
	Power   *float64 // W
	Total   *float64 // kWh
}

// Sensor holds what the bridge uses from a Tasmota sensor payload (the
// StatusSNS of a Status 0 response or an MQTT SENSOR message).
type Sensor struct {
	// Meters has one entry per energy meter. A device with a single meter
	// reports plain numbers in ENERGY; one with several reports arrays, one
	// element per meter. A plain number alongside arrays applies to every
	// meter.
	Meters []MeterReading
	// DeviceTemp is the device's own temperature in Celsius, from the ESP32
	// internal sensor, or nil. Ambient sensors (DS18B20 and the like) are not
	// device temperatures and are ignored.
	DeviceTemp *float64
}

// ParseSensor parses a Tasmota sensor payload. An empty payload gives an
// empty Sensor.
func ParseSensor(raw []byte) (*Sensor, error) {
	sensor := &Sensor{}
	if len(raw) == 0 {
		return sensor, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}

	if energy, found := top["ENERGY"]; found {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(energy, &fields); err != nil {
			return nil, fmt.Errorf("ENERGY: %w", err)
		}
		keys := []string{"Voltage", "Current", "Power", "Total"}
		nums := make([][]float64, len(keys))
		arrays := make([]bool, len(keys))
		meters := 0
		for i, key := range keys {
			val, found := fields[key]
			if !found {
				continue
			}
			var err error
			nums[i], arrays[i], err = parseNumbers(val)
			if err != nil {
				return nil, fmt.Errorf("ENERGY.%s: %w", key, err)
			}
			if arrays[i] {
				meters = max(meters, len(nums[i]))
			} else {
				meters = max(meters, 1)
			}
		}
		for m := range meters {
			var reading MeterReading
			for i, dst := range []**float64{&reading.Voltage, &reading.Current, &reading.Power, &reading.Total} {
				switch {
				case nums[i] == nil:
				case arrays[i] && m < len(nums[i]):
					*dst = &nums[i][m]
				case !arrays[i]:
					*dst = &nums[i][0]
				}
			}
			sensor.Meters = append(sensor.Meters, reading)
		}
	}

	if esp, found := top["ESP32"]; found {
		var fields struct{ Temperature *float64 }
		if err := json.Unmarshal(esp, &fields); err != nil {
			return nil, fmt.Errorf("ESP32: %w", err)
		}
		if fields.Temperature != nil {
			temp := *fields.Temperature
			var unit string
			if val, found := top["TempUnit"]; found {
				_ = json.Unmarshal(val, &unit)
			}
			if strings.EqualFold(unit, "F") {
				temp = (temp - 32) * 5 / 9
			}
			sensor.DeviceTemp = &temp
		}
	}

	return sensor, nil
}

// parseNumbers parses a JSON number or array of numbers.
func parseNumbers(raw json.RawMessage) (nums []float64, array bool, err error) {
	if err := json.Unmarshal(raw, &nums); err == nil {
		return nums, true, nil
	}
	var num float64
	if err := json.Unmarshal(raw, &num); err != nil {
		return nil, false, err
	}
	return []float64{num}, false, nil
}

// meterEndpoint returns the endpoint a meter belongs to. A device with one
// meter and one power channel has it on that channel's endpoint "1". One
// meter on any other device measures the whole device, so it goes on the root
// endpoint. With several meters, meter i belongs to channel i+1.
func meterEndpoint(meter, meters, channels int) (string, bool) {
	switch {
	case meters == 1 && channels == 1:
		return "1", true
	case meters == 1:
		return bridges.RootEndpointID, true
	case meter < channels:
		return strconv.Itoa(meter + 1), true
	default:
		return "", false
	}
}

// TasmotaDevice is one Tasmota device. Its endpoints depend on how many power
// channels the device has, which is only known once it has answered a status
// request, so the Woodhouse device is built and added to the bridge on the
// first successful poll.
type TasmotaDevice struct {
	log       *log.Context
	mac       string
	ip        string
	id        string
	addDevice func(dev *bridges.Device) error
	command   func(ctx context.Context, command string) ([]byte, error)

	dev    *bridges.Device // nil until built
	outs   map[int]*services.OnOff
	meters []*services.PowerMeter // one per sensor meter, nil where the meter has no endpoint
	temp   *services.DeviceTemperature
}

// NewTasmotaDevice creates a device for the Tasmota at ip with the given
// MAC (lowercase, no separators). It is added to the bridge on the first
// successful poll.
func NewTasmotaDevice(client *bridges.Bridge, mac, ip string) (*TasmotaDevice, error) {
	return newTasmotaDevice(client.AddDevice, mac, ip, func(ctx context.Context, command string) ([]byte, error) {
		return GetCommand(ctx, ip, command, false)
	})
}

func newTasmotaDevice(addDevice func(*bridges.Device) error, mac, ip string, command func(context.Context, string) ([]byte, error)) (*TasmotaDevice, error) {
	id, err := bridges.DeviceID("tasmota", mac)
	if err != nil {
		return nil, fmt.Errorf("device %q: %w", mac, err)
	}
	dev := &TasmotaDevice{
		log:       log.NewContext(log.DefaultLogger, id, log.DebugLevel),
		mac:       mac,
		ip:        ip,
		id:        id,
		addDevice: addDevice,
		command:   command,
	}
	dev.log.Infof("created device at %s", ip)
	return dev, nil
}

// build creates the Woodhouse device from the channels the device reported
// and adds it to the bridge. Endpoints and services are fixed from here on;
// restart the bridge after changing the device's configuration.
func (dev *TasmotaDevice) build(status *StatusResponse, power map[int]bool, sensor *Sensor) error {
	d := bridges.NewDevice(dev.id, clientsapi.Device_DEVICE)
	outs := make(map[int]*services.OnOff, len(power))

	for _, ch := range sortedChannels(power) {
		ep := d.Endpoint(strconv.Itoa(ch))
		ep.SetLabel(channelLabel(status.Status.FriendlyName, ch))

		out := services.NewOnOff("")
		out.On.OnWrite(func(on bool) error { return dev.setPower(ch, out, onOffString(on)) })
		out.OnToggle(func() error { return dev.setPower(ch, out, "toggle") })
		ep.AddService(out)
		outs[ch] = out
	}

	meters := make([]*services.PowerMeter, len(sensor.Meters))
	for i := range sensor.Meters {
		id, ok := meterEndpoint(i, len(sensor.Meters), len(power))
		if !ok {
			dev.log.Warnf("ignoring energy meter %d: no power channel for it", i+1)
			continue
		}
		meters[i] = services.NewPowerMeter("")
		d.Endpoint(id).AddService(meters[i])
	}

	var temp *services.DeviceTemperature
	if sensor.DeviceTemp != nil {
		temp = services.NewDeviceTemperature("")
		d.AddService(temp)
	}

	if err := dev.addDevice(d); err != nil {
		d.Close()
		return err
	}
	dev.dev = d
	dev.outs = outs
	dev.meters = meters
	dev.temp = temp
	dev.log.Infof("added device with %d power channel(s), %d energy meter(s)", len(outs), len(sensor.Meters))
	return nil
}

// channelLabel is the device's FriendlyName for the channel if it has one,
// else "Output N".
func channelLabel(friendlyNames []string, ch int) string {
	if ch <= len(friendlyNames) && friendlyNames[ch-1] != "" {
		return friendlyNames[ch-1]
	}
	return fmt.Sprintf("Output %d", ch)
}

// setPower sends a Power command (state is "on", "off" or "toggle") for the
// channel and applies the state the device answers with.
func (dev *TasmotaDevice) setPower(ch int, out *services.OnOff, state string) error {
	body, err := dev.command(context.Background(), fmt.Sprintf("Power%d%%20%s", ch, state))
	if err != nil {
		return fmt.Errorf("set power: %w", err)
	}
	power, err := ParsePower(body)
	if err != nil {
		return fmt.Errorf("set power: %w", err)
	}
	on, found := power[ch]
	if !found {
		return fmt.Errorf("set power: no state for channel %d in response %q", ch, body)
	}
	if out.On.Set(on) {
		dev.log.Infof("via request: channel %d power changed to %s", ch, onOffString(on))
	}
	return nil
}

func onOffString(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func (dev *TasmotaDevice) PollStatus(ctx context.Context) {
	status, err := GetStatus(ctx, dev.ip, false)
	if err != nil {
		// Assume offline.
		if dev.dev != nil && dev.dev.Online.Online.Set(false) {
			dev.log.Infof("went offline")
		}
		return
	}
	dev.applyStatus(status)
}

// applyStatus applies a Status 0 response, building the device if needed.
func (dev *TasmotaDevice) applyStatus(status *StatusResponse) {
	power, err := ParsePower(status.StatusSTS)
	if err != nil {
		dev.log.Errorf("failed to parse state: %s", err)
		return
	}

	sensor, err := ParseSensor(status.StatusSNS)
	if err != nil {
		// Sensors are optional; carry on with the power state.
		dev.log.Errorf("failed to parse sensors: %s", err)
		sensor = &Sensor{}
	}

	if dev.dev == nil {
		if err := dev.build(status, power, sensor); err != nil {
			dev.log.Errorf("failed to add device: %s", err)
			return
		}
	}

	if dev.dev.Info.Name.Set(status.Name()) {
		dev.log.Infof("name changed to %q", status.Name())
	}
	dev.dev.Online.LastSeen.Set(time.Now())
	if dev.dev.Online.Online.Set(true) {
		dev.log.Infof("came online")
	}
	dev.applyPower(power)
	dev.applySensor(sensor)
}

// applySensor applies energy and temperature readings. Readings the device
// did not build a service for (see build) are ignored.
func (dev *TasmotaDevice) applySensor(sensor *Sensor) {
	for i, reading := range sensor.Meters {
		if i >= len(dev.meters) || dev.meters[i] == nil {
			continue
		}
		meter := dev.meters[i]
		if reading.Voltage != nil {
			meter.Voltage.Set(*reading.Voltage)
		}
		if reading.Current != nil {
			meter.Current.Set(*reading.Current)
		}
		if reading.Power != nil {
			meter.Power.Set(*reading.Power)
		}
		if reading.Total != nil {
			meter.Energy.Set(*reading.Total)
		}
	}
	if sensor.DeviceTemp != nil && dev.temp != nil {
		dev.temp.Temperature.Set(*sensor.DeviceTemp)
	}
}

func (dev *TasmotaDevice) applyPower(power map[int]bool) {
	for ch, on := range power {
		out, found := dev.outs[ch]
		if !found {
			continue
		}
		if out.On.Set(on) {
			dev.log.Infof("channel %d power changed to %s", ch, onOffString(on))
		}
	}
}
