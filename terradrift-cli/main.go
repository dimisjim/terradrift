package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/olekukonko/tablewriter"
	"github.com/rootsami/terradrift/pkg/config"
	"github.com/rootsami/terradrift/pkg/tfstack"
	kingpin "gopkg.in/alecthomas/kingpin.v2"
	"gopkg.in/yaml.v2"

	log "github.com/sirupsen/logrus"
)

var (
	app              = kingpin.New("terradrift-cli", "A command-line tool to detect drifts in terraform IaC")
	workdir          = app.Flag("workdir", "workdir of a project that contains all terraform directories").Default("./").String()
	configPath       = app.Flag("config", "Path for configuration file holding the stack information").String()
	extraBackendVars = app.Flag("extra-backend-vars", "Extra backend environment variables ex. GOOGLE_CREDENTIALS, AWS_ACCESS_KEY_ID or AWS_CONFIG_FILE ..etc").StringMap()
	debug            = app.Flag("debug", "Enable debug mode").Default("false").Bool()
	generateConfig   = app.Flag("generate-config-only", "Generate a config file based on a provided workspace").Default("false").Bool()
	output           = app.Flag("output", "Output format supported: json, yaml and table").Default("table").Enum("table", "json", "yaml")
	maxParallelPlans = app.Flag("max-parallel-plans", "Maximum number of parallel plans to execute").Default("0").Int() // 0 indicates all projects
	showProgress     = app.Flag("show-progress", "Show the progress output with workers").Default("true").Bool()
	version          string
)

type stackOutput struct {
	Name    string `json:"name" yaml:"name"`
	Path    string `json:"path" yaml:"path"`
	Drift   string `json:"drift" yaml:"drift"` // Modified to string for "N/A"
	Add     string `json:"add" yaml:"add"`
	Change  string `json:"change" yaml:"change"`
	Destroy string `json:"destroy" yaml:"destroy"`
	TFver   string `json:"tfver" yaml:"tfver"`
	Failure string `json:"failure" yaml:"failure"`
}

func init() {
	app.Version(version)
	kingpin.MustParse(app.Parse(os.Args[1:]))

	if *debug {
		log.SetLevel(log.DebugLevel)
	}

	// If workdir path is not absolute, make it absolute and add a trailing slash
	if !path.IsAbs(*workdir) {
		absPath, err := filepath.Abs(*workdir)
		if err != nil {
			log.Fatalf("Error getting absolute path for workdir: %s", err)
		}
		*workdir = absPath + "/"
	} else if !strings.HasSuffix(*workdir, "/") {
		*workdir = *workdir + "/"
	}
}

// Function to render the table in the console
func renderStatusTable(workerStatus map[int]string, plannedCount, errorCount, totalStacks int) {
	fmt.Print("\033[H\033[2J") // Clear the screen

	// Print header
	fmt.Println("Worker #\tCurrently Planned Project")
	fmt.Println("---------------------------------------")

	// Sort worker IDs for ordered output
	keys := make([]int, 0, len(workerStatus))
	for k := range workerStatus {
		keys = append(keys, k)
	}
	sort.Ints(keys)

	for _, workerID := range keys {
		fmt.Printf("  %d\t\t%s\n", workerID+1, workerStatus[workerID])
	}

	// Print progress line
	fmt.Printf("\nPlanned projects: %d/%d\n", plannedCount, totalStacks)
	fmt.Printf("Errored projects: %d\n", errorCount)
}

func main() {
	var cfg *config.Config
	var stackOutputs []stackOutput
	var errors []string // Collect error messages here
	var err error

	// Load configuration based on the provided flags
	switch {
	case *configPath != "":
		log.WithFields(log.Fields{"config": *configPath}).Debug("loading config file")
		cfg, err = config.ConfigLoader(*workdir, *configPath)
		if err != nil {
			log.Fatalf("Error loading config file: %s", err)
		}
	case *generateConfig:
		cfg, err = config.ConfigGenerator(*workdir)
		if err != nil {
			log.Fatalf("Error generating config file: %s", err)
		}
		outputWriter(cfg, "yaml")
		os.Exit(0)
	case *configPath == "":
		log.Debug("config file not found, running stack init on each directory that contains .tf files")
		cfg, err = config.ConfigGenerator(*workdir)
		if err != nil {
			log.Fatalf("error generating config file: %s", err)
		}
	}

	// If maxParallelPlans is set to 0, set it to the total number of stacks (i.e., run all projects in parallel)
	if *maxParallelPlans == 0 {
		*maxParallelPlans = len(cfg.Stacks)
	}

	// Set up worker pool with a limit on parallelism
	totalStacks := len(cfg.Stacks)
	stackChan := make(chan config.Stack, totalStacks)
	var wg sync.WaitGroup

	workerStatus := make(map[int]string)
	var mu sync.Mutex
	plannedCount := 0
	errorCount := 0

	// Start workers based on maxParallelPlans
	for i := 0; i < *maxParallelPlans; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for stack := range stackChan {
				// Update the worker's status
				mu.Lock()
				workerStatus[workerID] = stack.Name
				if *showProgress {
					renderStatusTable(workerStatus, plannedCount, errorCount, totalStacks)
				}
				mu.Unlock()

				// Execute stack planning
				response, tfver, err := tfstack.StackInit(*workdir, stack, *extraBackendVars)
				var output stackOutput

				// Check for errors
				if err != nil {
					mu.Lock()
					errorCount++
					errors = append(errors, fmt.Sprintf("Project: %s, Error: %s", stack.Name, err.Error()))
					mu.Unlock()

					// Mark as errored with "N/A" fields
					output = stackOutput{
						Name:    stack.Name,
						Path:    stack.Path,
						Drift:   "N/A",
						Add:     "N/A",
						Change:  "N/A",
						Destroy: "N/A",
						TFver:   "N/A",
						Failure: "true",
					}
				} else {
					// If no error, fill the stackOutput with actual data
					output = stackOutput{
						Name:    stack.Name,
						Path:    stack.Path,
						Drift:   strconv.FormatBool(response.Drift),
						Add:     strconv.Itoa(response.Add),
						Change:  strconv.Itoa(response.Change),
						Destroy: strconv.Itoa(response.Destroy),
						TFver:   tfver,
						Failure: "false",
					}
				}

				// Collect output
				mu.Lock()
				stackOutputs = append(stackOutputs, output)
				plannedCount++
				workerStatus[workerID] = "Idle"
				if *showProgress {
					renderStatusTable(workerStatus, plannedCount, errorCount, totalStacks)
				}
				mu.Unlock()
			}
		}(i)
	}

	// Send stacks to the channel for processing
	for _, stack := range cfg.Stacks {
		stackChan <- stack
	}
	close(stackChan) // Close the channel when done sending stacks

	// Wait for all workers to complete
	wg.Wait()

	// Print errors if any
	if len(errors) > 0 {
		fmt.Println("\nEncountered Errors:")
		for _, e := range errors {
			fmt.Println(e)
		}
	}

	// Sort final output alphabetically
	sort.Slice(stackOutputs, func(i, j int) bool {
		return stackOutputs[i].Name < stackOutputs[j].Name
	})

	// Output the results based on the output flag
	switch *output {
	case "json":
		outputWriter(stackOutputs, "json")
	case "yaml":
		outputWriter(stackOutputs, "yaml")
	case "table":
		tableWriter(stackOutputs)
	}
}

func tableWriter(stackOutputs []stackOutput) {
	columns := []string{"STACK-NAME", "DRIFT", "ADD", "CHANGE", "DESTROY", "PATH", "TF-VERSION", "FAILURE"}
	var data [][]string

	for _, stackOutput := range stackOutputs {
		row := []string{
			stackOutput.Name,
			stackOutput.Drift,
			stackOutput.Add,
			stackOutput.Change,
			stackOutput.Destroy,
			stackOutput.Path,
			stackOutput.TFver,
			stackOutput.Failure,
		}
		data = append(data, row)
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader(columns)
	table.SetAutoWrapText(false)
	table.SetAutoFormatHeaders(true)
	table.SetHeaderAlignment(3)
	table.SetAlignment(3)
	table.SetCenterSeparator("")
	table.SetColumnSeparator("")
	table.SetRowSeparator("")
	table.SetHeaderLine(false)
	table.SetBorder(false)
	table.SetTablePadding("\t")
	table.SetNoWhiteSpace(true)
	table.AppendBulk(data)
	table.Render()
}

func outputWriter(data interface{}, format string) {
	switch format {
	case "json":
		o, err := json.Marshal(data)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Print(string(o))
	case "yaml":
		o, err := yaml.Marshal(data)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Print(string(o))
	}
}
