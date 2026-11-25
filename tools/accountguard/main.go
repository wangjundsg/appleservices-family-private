package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Laky-64/appleservices/internal/accountguard"
)

func main() {
	dir := flag.String("dir", "", "exact account state directory")
	release := flag.Bool("release", false, "release local protection without starting login")
	confirmed := flag.Bool("confirm-recovered", false, "owner confirms recovery through a trusted official flow")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "account directory required")
		os.Exit(2)
	}
	if *release {
		if err := accountguard.Release(*dir, *confirmed); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("Local protection released; no worker or authentication was started.")
		return
	}
	if accountguard.Check(*dir) != nil {
		fmt.Println("protected")
		return
	}
	fmt.Println("not protected; this does not prove Apple account recovery")
}
