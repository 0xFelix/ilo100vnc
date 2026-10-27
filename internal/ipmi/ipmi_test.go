package ipmi

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/bougou/go-ipmi/pkg/bmc"
	"github.com/bougou/go-ipmi/pkg/clock"
	"github.com/bougou/go-ipmi/pkg/hal"
	"github.com/bougou/go-ipmi/pkg/hal/mock"
	"github.com/bougou/go-ipmi/pkg/server"
	"github.com/bougou/go-ipmi/pkg/transport/udp"
	"github.com/bougou/go-ipmi/pkg/types"
)

// recChassis records chassis actions of go-ipmi's reference BMC.
type recChassis struct {
	*mock.Chassis
	ops chan string
}

func (c recChassis) SetPower(ctx context.Context, on bool) error {
	if on {
		c.ops <- "on"
	} else {
		c.ops <- "off"
	}
	return c.Chassis.SetPower(ctx, on)
}

func (c recChassis) PowerCycle(ctx context.Context) error {
	c.ops <- "cycle"
	return c.Chassis.PowerCycle(ctx)
}

func (c recChassis) ColdReset(ctx context.Context) error {
	c.ops <- "reset"
	return c.Chassis.ColdReset(ctx)
}

func (c recChassis) WarmReset(ctx context.Context) error {
	c.ops <- "soft"
	return c.Chassis.WarmReset(ctx)
}

type recHAL struct {
	*mock.HAL
	ch recChassis
}

func (h recHAL) Chassis() hal.ChassisHAL { return h.ch }

func startBMC(t *testing.T) (recChassis, int) {
	t.Helper()
	m := mock.New()
	ch := recChassis{Chassis: m.Chassis().(*mock.Chassis), ops: make(chan string, 4)}
	b := bmc.New(bmc.DeviceInfo{IPMIVersion: 0x20}, [16]byte{}, recHAL{HAL: m, ch: ch}, bmc.WithClock(clock.Real))
	u, err := b.Users.Add(2, "admin")
	if err != nil {
		t.Fatal(err)
	}
	u.SetPassword([]byte("secret"))
	u.Enabled = true
	u.ChannelAccess[1] = bmc.UserChannelAccess{MaxPrivilege: bmc.PrivilegeLevelAdministrator, Enabled: true}

	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	srv := server.NewServer(b, udp.Wrap(conn, udp.WithReadTimeout(100*time.Millisecond)))
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx) //nolint:errcheck
	t.Cleanup(func() { cancel(); _ = srv.Close() })
	return ch, conn.LocalAddr().(*net.UDPAddr).Port
}

func next(t *testing.T, ch recChassis) string {
	t.Helper()
	select {
	case op := <-ch.ops:
		return op
	case <-time.After(5 * time.Second):
		t.Fatal("no chassis action")
		return ""
	}
}

func TestChassisActions(t *testing.T) {
	ch, port := startBMC(t)
	c := New("127.0.0.1", port, "admin", "secret", nil)
	ctx := context.Background()
	for _, tc := range []struct {
		f    func(context.Context) error
		want string
	}{
		{c.PowerOn, "on"},
		{c.SoftOff, "soft"},
		{c.PowerCycle, "cycle"},
		{c.Reset, "reset"},
	} {
		if err := tc.f(ctx); err != nil {
			t.Fatalf("%s: %v", tc.want, err)
		}
		if got := next(t, ch); got != tc.want {
			t.Fatalf("got %s want %s", got, tc.want)
		}
	}
}

func TestBIOSSetup(t *testing.T) {
	ch, port := startBMC(t)
	c := New("127.0.0.1", port, "admin", "secret", nil)
	ctx := context.Background()

	// off: set the flag and power on
	if err := c.BIOSSetup(ctx); err != nil {
		t.Fatal(err)
	}
	if got := next(t, ch); got != "on" {
		t.Fatalf("got %s want on", got)
	}
	f, err := ch.GetBootFlags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !f.BootFlagsValid || f.Persist || f.BIOSBootType != types.BIOSBootTypeLegacy ||
		f.BootDeviceSelector != types.BootDeviceSelectorForceBIOSSetup {
		t.Fatalf("boot flags %+v", f)
	}
	if b := f.Pack(); b[0] != 0x80 || b[1] != 0x18 { // as `ipmitool chassis bootdev bios`
		t.Fatalf("boot flags % X", b)
	}
	ack, err := ch.GetBootInfoAcknowledge(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ack.ByBIOSPOST {
		t.Fatalf("boot info acknowledge %+v", ack)
	}

	// on: reset
	if err := c.BIOSSetup(ctx); err != nil {
		t.Fatal(err)
	}
	if got := next(t, ch); got != "reset" {
		t.Fatalf("got %s want reset", got)
	}
}

func TestWrongPassword(t *testing.T) {
	_, port := startBMC(t)
	c := New("127.0.0.1", port, "admin", "wrong", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.PowerOn(ctx); err == nil {
		t.Fatal("no error")
	}
}
