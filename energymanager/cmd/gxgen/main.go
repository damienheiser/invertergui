// gxgen generates a GX data file for VRM registration
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/diebietse/invertergui/energymanager/vrm"
)

func main() {
	portalID := flag.String("portal", "", "VRM Portal ID (required)")
	name := flag.String("name", "EnergyMonitor", "Device name for VRM")
	output := flag.String("output", "gxdata.json", "Output file path")
	flag.Parse()

	if *portalID == "" {
		fmt.Fprintln(os.Stderr, "Error: -portal flag is required")
		fmt.Fprintln(os.Stderr, "Usage: gxgen -portal <portal_id> [-name <device_name>] [-output <file>]")
		os.Exit(1)
	}

	log.Printf("Generating GX file for portal ID: %s", *portalID)
	log.Printf("Device name: %s", *name)
	log.Printf("Output: %s", *output)

	if err := vrm.GenerateGXFile(*portalID, *name, *output); err != nil {
		log.Fatalf("Failed to generate GX file: %v", err)
	}

	log.Printf("GX file generated successfully: %s", *output)
	log.Println("")
	log.Println("To register with VRM:")
	log.Println("1. Go to https://vrm.victronenergy.com")
	log.Println("2. Click 'Add installation' or 'Replace GX device'")
	log.Println("3. Upload this file")
}
