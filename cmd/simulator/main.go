/*

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command simulator replays recorded Cloud Spanner metrics against candidate
// SpannerAutoscaler configurations, so scaling-config changes (min PU, step
// sizes, scale-down windows, schedules, …) can be evaluated for cost and CPU
// risk before touching production.
//
// Subcommands:
//
//	fetch     download CPU / processing-units metrics from Cloud Monitoring into a CSV
//	simulate  replay one configuration against a metrics CSV
//	compare   replay several configurations against the same metrics CSV
package main

import (
	"fmt"
	"os"

	"github.com/go-logr/logr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

func main() {
	// The manifest loader reuses the production defaulting webhook, which
	// logs through controller-runtime; silence it for CLI usage.
	logf.SetLogger(logr.Discard())

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "simulate":
		err = runSimulate(os.Args[2:])
	case "compare":
		err = runCompare(os.Args[2:])
	case "recommend":
		err = runRecommend(os.Args[2:])
	case "fetch":
		err = runFetch(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Usage: simulator <subcommand> [flags]

Subcommands:
  fetch      download CPU / processing-units metrics from Cloud Monitoring into a CSV
  simulate   replay one SpannerAutoscaler configuration against a metrics CSV
  compare    replay several configurations against the same metrics CSV
  recommend  grid-search configuration candidates and rank the safe ones by cost

Run 'simulator <subcommand> -h' for the flags of each subcommand.
`)
}
