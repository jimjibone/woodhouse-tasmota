package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jimjibone/log"
	"github.com/jimjibone/wh/v2/bridges"
	"github.com/jimjibone/wh/v2/clients"
	"github.com/jimjibone/wh/v2/shared/stores"
	"github.com/urfave/cli/v2"
)

type JsonNumber float64

func (js *JsonNumber) UnmarshalJSON(p []byte) error {
	// Try to unmarshal as number.
	var num float64
	if err := json.Unmarshal(p, &num); err == nil {
		*js = JsonNumber(num)
		return nil
	}

	// Try to unmarshal as string.
	var s string
	if err := json.Unmarshal(p, &s); err == nil {
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		*js = JsonNumber(n)
		return nil
	}

	return fmt.Errorf("invalid float value: %s", p)
}

type StatusResponse struct {
	Status struct {
		Module       int
		DeviceName   string
		FriendlyName []string
		Topic        string
		ButtonTopic  string
		Power        JsonNumber
		PowerLock    JsonNumber
		PowerOnState int
		LedState     int
		LedMask      string
		SaveData     int
		SaveState    int
		SwitchTopic  string
		SwitchMode   []int
		ButtonRetain int
		SwitchRetain int
		SensorRetain int
		PowerRetain  int
		InfoRetain   int
		StateRetain  int
	}
	StatusNET struct {
		Hostname   string
		IPAddress  string
		Gateway    string
		Subnetmask string
		DNSServer  string
		Mac        string
		Webserver  int
		WifiConfig int
		WifiPower  float64
	}

	// StatusSTS has the same shape as a Tasmota STATE message (POWER, POWER1,
	// POWER2, ...). StatusSNS has the same shape as a SENSOR message (ENERGY,
	// ESP32, ...). Both are absent on devices that do not report them.
	StatusSTS json.RawMessage
	StatusSNS json.RawMessage
}

func (sr *StatusResponse) Name() string {
	name := sr.Status.Topic
	if len(sr.Status.FriendlyName) > 0 {
		name = sr.Status.FriendlyName[0]
	}
	return name
}

func (sr *StatusResponse) ID() string {
	id := sr.StatusNET.Mac
	id = strings.ToLower(strings.Join(strings.Split(id, ":"), ""))
	return id
}

func (sr *StatusResponse) String() string {
	power, _ := ParsePower(sr.StatusSTS)
	channels := make([]string, 0, len(power))
	for _, ch := range sortedChannels(power) {
		state := "off"
		if power[ch] {
			state = "on"
		}
		channels = append(channels, fmt.Sprintf("%d=%s", ch, state))
	}
	return fmt.Sprintf("id: %q, name: %q, mac: %s, power: [%s]", sr.ID(), sr.Name(), sr.StatusNET.Mac, strings.Join(channels, " "))
}

func GetCommand(ctx context.Context, endpoint, command string, debug bool) ([]byte, error) {
	url := fmt.Sprintf("http://%s/cm?cmnd=%s", endpoint, command)
	if debug {
		log.Debugf("GET %s", url)
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	req = req.WithContext(ctx)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

func GetStatus(ctx context.Context, endpoint string, debug bool) (*StatusResponse, error) {
	resp, err := GetCommand(ctx, endpoint, `Status%200`, debug)
	if err != nil {
		return nil, err
	}

	if debug {
		log.Debugf("status response: %s", resp)
	}

	status := &StatusResponse{}
	err = json.Unmarshal(resp, status)
	if err != nil {
		return nil, err
	}

	return status, nil
}

func main() {
	app := &cli.App{
		Name:  "woodhouse-tasmota",
		Usage: "Woodhouse client for Tasmota devices.",
		Flags: []cli.Flag{
			&cli.PathFlag{
				Name:     "store",
				Usage:    "path to config storage location",
				Required: true,
			},
			&cli.StringFlag{
				Name:     "addr",
				Usage:    "woodhouse-core server address",
				Required: true,
			},
			&cli.StringFlag{
				Name:  "id",
				Usage: "ID used by this bridge",
				Value: "tasmota",
			},
			&cli.BoolFlag{
				Name:    "debug",
				Aliases: []string{"v"},
				Usage:   "enable debug logging",
			},
		},
		Commands: []*cli.Command{
			{
				Name:      "add-device",
				Usage:     "Add a Tasmota device by its IP",
				ArgsUsage: "<device-ip-1> <device-ip-2...>",
				Action: func(args *cli.Context) error {
					// Create the store.
					store := stores.NewFSStore(args.String("store"))

					// Load previous endpoints.
					endpoints := make(map[string]string) // key=id (mac), value=ip
					if store.Has("endpoints") {
						err := stores.GetJson(store, "endpoints", &endpoints)
						if err != nil {
							return fmt.Errorf("failed to get previous endpoints: %w", err)
						}
					}

					// Parse remaining args for the device IP address.
					ips := args.Args().Slice()
					if len(ips) == 0 {
						return fmt.Errorf("no IP address provided")
					}
					for _, rawIP := range ips {
						// Check that this is a valid IP.
						ip := net.ParseIP(rawIP)
						if ip == nil {
							return fmt.Errorf("invalid IP address %q", rawIP)
						}
						ipString := ip.String()
						log.Infof("connecting to %s", ipString)

						// Check that we can connect to this IP.
						status, err := GetStatus(context.Background(), ipString, true)
						if err != nil {
							return err
						}
						log.Infof("status: %v", status)

						// Add the IP to the list if it's new.
						endpoints[status.ID()] = ipString
					}

					// Save the new endpoints.
					err := stores.SetJson(store, "endpoints", endpoints)
					if err != nil {
						return err
					}

					return nil
				},
			},
		},
		Before: func(args *cli.Context) error {
			// Setup logging.
			if args.Bool("debug") {
				log.SetOptions(log.WithMinLevel(log.DebugLevel))
			} else {
				log.SetOptions(log.WithMinLevel(log.InfoLevel))
			}
			return nil
		},
		Action: func(args *cli.Context) error {
			// Create the store.
			store := stores.NewFSStore(args.String("store"))

			// Load previous endpoints.
			endpoints := make(map[string]string) // key=id (mac), value=ip
			if store.Has("endpoints") {
				err := stores.GetJson(store, "endpoints", &endpoints)
				if err != nil {
					return fmt.Errorf("failed to get previous endpoints: %w", err)
				}
			}

			// Create the client.
			client := bridges.NewBridge(bridges.BridgeConfig{
				ClientConfig: clients.ClientConfig{
					Store:             store,
					ServerAddr:        args.String("addr"),
					ClientID:          args.String("id"),
					ClientName:        "Tasmota Bridge",
					ClientDescription: "Bridge for Tasmota devices",
					ClientVersion:     "0.1.0",
				},
			})

			// Set up devices.
			var devices []*TasmotaDevice
			for mac, ip := range endpoints {
				dev, err := NewTasmotaDevice(client, mac, ip)
				if err != nil {
					return err
				}
				dev.PollStatus(context.Background())
				devices = append(devices, dev)
			}

			// Start the Tasmota goroutine.
			wg := &sync.WaitGroup{}
			defer wg.Wait()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wg.Add(1)
			go func() {
				defer wg.Done()
				ticker := time.NewTicker(2 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return

					case <-ticker.C:
						// Poll devices.
						for _, dev := range devices {
							dev.PollStatus(ctx)
						}
					}
				}
			}()

			// // Run the client.
			err := client.Run()
			if err != nil {
				return err
			}

			return nil
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatalln(err)
	}
}
