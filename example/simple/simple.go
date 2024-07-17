package main

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"os/signal"
	"syscall"
	"time"

	dbus "github.com/godbus/dbus/v5"

	velib "github.com/bakins/go-velib"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil {
		log.Fatalf("error: %v", err)
	}
}

func run(ctx context.Context) error {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return fmt.Errorf("failed to connect to system bus: %w", err)
	}

	defer conn.Close()

	deviceName := "testing_abc_123"
	serviceName := "com.victronenergy.battery." + deviceName

	service, err := velib.NewService(conn, serviceName)
	if err != nil {
		return fmt.Errorf("failed to create service: %w", err)
	}

	defer service.Close()

	deviceInstance, err := service.GetDeviceInstance()
	if err != nil {
		return fmt.Errorf("failed to get device instance: %w", err)
	}

	paths := map[string]any{
		"Connected":           1,
		"CustomName":          "go-velib" + deviceName,
		"DeviceName":          deviceName,
		"ErrorCode":           0,
		"FirmwareVersion":     "0.1.0",
		"Mgmt/Connection":     deviceName,
		"Mgmt/ProcessName":    "go-velib",
		"Mgmt/ProcessVersion": "0.1.0",
		"ProductId":           65535,
		"ProductName":         "go-velib",
		"HardwareVersion":     "0.1.0",
		"DeviceInstance":      deviceInstance,
	}

	for path, value := range paths {
		if _, err := service.AddPath("/"+path, value); err != nil {
			return fmt.Errorf("failed to add path %s: %w", path, err)
		}
	}

	voltage, err := service.AddPath(
		"/Dc/0/Voltage",
		velib.NewFormatterObject(
			0.0,
			func(a any) string {
				return fmt.Sprintf("%.2f V", a.(float64))
			},
		),
	)
	if err != nil {
		return fmt.Errorf("failed to add path: %w", err)
	}

	if err := service.Register(); err != nil {
		return fmt.Errorf("failed to register service: %w", err)
	}

	log.Printf("service registered: %s", serviceName)

	if err := voltage.SetValue(rand.Float64() * 10); err != nil {
		return fmt.Errorf("failed to set value: %w", err)
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := voltage.SetValue(rand.Float64() * 10); err != nil {
				return fmt.Errorf("failed to set value: %w", err)
			}
		}
	}
}
