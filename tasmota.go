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

	dev  *bridges.Device // nil until built
	outs map[int]*services.OnOff
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
func (dev *TasmotaDevice) build(status *StatusResponse, power map[int]bool) error {
	d := bridges.NewDevice(dev.id, clientsapi.Device_DEVICE)
	outs := make(map[int]*services.OnOff, len(power))

	for _, ch := range sortedChannels(power) {
		ep := d.Endpoint(strconv.Itoa(ch))
		ep.SetLabel(channelLabel(status.Status.FriendlyName, ch))

		out := services.NewOnOff("")
		out.On.OnWrite(func(on bool) error {
			state := "off"
			if on {
				state = "on"
			}
			return dev.setPower(ch, out, state)
		})
		out.OnToggle(func() error { return dev.setPower(ch, out, "toggle") })
		ep.AddService(out)
		outs[ch] = out
	}

	if err := dev.addDevice(d); err != nil {
		d.Close()
		return err
	}
	dev.dev = d
	dev.outs = outs
	dev.log.Infof("added device with %d power channel(s)", len(outs))
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

	if dev.dev == nil {
		if err := dev.build(status, power); err != nil {
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
