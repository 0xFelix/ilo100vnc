// Package ipmi controls the host's power through the iLO's IPMI over LAN
// interface (IPMI 2.0, RMCP+).
package ipmi

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bougou/go-ipmi/pkg/client"
	"github.com/bougou/go-ipmi/pkg/command/chassis"
	"github.com/bougou/go-ipmi/pkg/types"
)

// Port is the default RMCP+ UDP port.
const Port = 623

// Client runs chassis commands, one session per command.
type Client struct {
	host, user, password string
	port                 int
	log                  *slog.Logger

	mu sync.Mutex // one command at a time
}

// New creates a client for the BMC at host:port.
func New(host string, port int, user, password string, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{host: host, port: port, user: user, password: password, log: log}
}

// do opens a session, runs f and closes the session.
func (c *Client) do(ctx context.Context, f func(context.Context, *client.Client) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cl, err := client.NewClient(c.host, c.port, c.user, c.password)
	if err != nil {
		return err
	}
	if err := cl.Connect(ctx); err != nil {
		return fmt.Errorf("ipmi connect: %w", err)
	}
	defer func() {
		if err := cl.Close(ctx); err != nil {
			c.log.Debug("ipmi close", "err", err)
		}
	}()
	return f(ctx, cl)
}

func (c *Client) control(ctx context.Context, ctl chassis.ChassisControl) error {
	return c.do(ctx, func(ctx context.Context, cl *client.Client) error {
		_, err := cl.ChassisControl(ctx, ctl)
		return err
	})
}

// SoftOff asks the OS to shut down (ACPI power button).
func (c *Client) SoftOff(ctx context.Context) error {
	return c.control(ctx, chassis.ChassisControlSoftShutdown)
}

// PowerCycle switches the host off and on again.
func (c *Client) PowerCycle(ctx context.Context) error {
	return c.control(ctx, chassis.ChassisControlPowerCycle)
}

// Reset hard-resets the host.
func (c *Client) Reset(ctx context.Context) error {
	return c.control(ctx, chassis.ChassisControlHardReset)
}

// PowerOn switches the host on.
func (c *Client) PowerOn(ctx context.Context) error {
	return c.control(ctx, chassis.ChassisControlPowerUp)
}

// PowerOff switches the host off without shutting down the OS.
func (c *Client) PowerOff(ctx context.Context) error {
	return c.control(ctx, chassis.ChassisControlPowerDown)
}

// BIOSSetup sets the one-time boot flag "force boot into BIOS Setup" and
// powers the host on, or resets it if it is running.
func (c *Client) BIOSSetup(ctx context.Context) error {
	return c.do(ctx, func(ctx context.Context, cl *client.Client) error {
		// Same sequence as `ipmitool chassis bootdev bios`.
		if err := setInProgress(ctx, cl, types.SetInProgress_SetInProgress); err != nil {
			c.log.Debug("ipmi set-in-progress not supported", "err", err)
		}
		defer func() { _ = setInProgress(ctx, cl, types.SetInProgress_SetComplete) }()
		if err := cl.SetSystemBootOptionsParamFor(ctx, &types.BootOptionParam_BootInfoAcknowledge{ByBIOSPOST: true}); err != nil {
			return fmt.Errorf("clear boot info acknowledge: %w", err)
		}
		if err := cl.SetBootDevice(ctx, types.BootDeviceSelectorForceBIOSSetup, types.BIOSBootTypeLegacy, false); err != nil {
			return err
		}
		_ = setInProgress(ctx, cl, types.SetInProgress_CommitWrite)

		st, err := cl.GetChassisStatus(ctx)
		if err != nil {
			return err
		}
		ctl := chassis.ChassisControlPowerUp
		if st.PowerIsOn {
			ctl = chassis.ChassisControlHardReset
		}
		_, err = cl.ChassisControl(ctx, ctl)
		return err
	})
}

func setInProgress(ctx context.Context, cl *client.Client, v types.SetInProgressState) error {
	return cl.SetSystemBootOptionsParamFor(ctx, &types.BootOptionParam_SetInProgress{Value: v})
}
