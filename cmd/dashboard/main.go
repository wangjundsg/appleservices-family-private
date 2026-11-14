package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/Laky-64/appleservices/internal/dashboard"
	"os"
	"os/signal"
	"path/filepath"
)

func main() {
	dir := flag.String("dir", "./state", "saved device and session directory")
	repo := flag.String("repo", ".", "Apple repository root for registration pipeline")
	worker := flag.String("worker", "./bin/trusted-device-gui.exe", "trusted-device executable")
	library := flag.String("adi-dll", "./.cache/openbubbles-store/rust_lib_bluebubbles.dll", "verified native module")
	python := flag.String("python", "python", "64-bit Python executable")
	macPushSetup := flag.String("mac-push-setup", "./tools/mac-push-check.py", "account-isolated Mac APS bootstrap")
	port := flag.Int("port", 18741, "loopback dashboard port")
	allowPurchase := flag.Bool("allow-purchase", false, "allow paid number purchases with an explicit configured budget")
	flag.Parse()
	absDir, _ := filepath.Abs(*dir)
	absRepo, _ := filepath.Abs(*repo)
	absWorker, _ := filepath.Abs(*worker)
	absLibrary, _ := filepath.Abs(*library)
	absMacPushSetup, _ := filepath.Abs(*macPushSetup)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	server, err := dashboard.New(dashboard.Config{Dir: absDir, Worker: absWorker, Library: absLibrary, Python: *python, MacPushSetup: absMacPushSetup, Port: *port, RepoRoot: absRepo, AllowPurchase: *allowPurchase})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	appleID, password := os.Getenv("APPLESERVICES_APPLEID"), os.Getenv("APPLESERVICES_PASSWORD")
	os.Unsetenv("APPLESERVICES_APPLEID")
	os.Unsetenv("APPLESERVICES_PASSWORD")
	server.Bootstrap(ctx, appleID, password)
	fmt.Println("Local dashboard:", server.URL())
	if err := server.Serve(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "Dashboard stopped")
		os.Exit(1)
	}
}
