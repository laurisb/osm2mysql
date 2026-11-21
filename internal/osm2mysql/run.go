package osm2mysql

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Process exit codes returned by Run.
const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 2
)

type options struct {
	pbf             string
	selections      tagSelections
	dbHost          string
	dbPort          int
	dbUser          string
	dbPass          string
	dbName          string
	tableName       string
	threads         int
	sortMem         int64
	centroidWorkers int
	showVersion     bool
}

// Run executes the osm2mysql command and exits the process with the
// corresponding exit code.
func Run(version string) {
	os.Exit(runCLI(version, os.Args[1:], os.Stdout, os.Stderr))
}

// runCLI is Run with injectable streams so the command line can be tested.
func runCLI(version string, args []string, stdout, stderr io.Writer) int {
	opts, fs, err := parseOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		printUsage(stdout, version, fs)
		return ExitOK
	}
	if err != nil {
		fmt.Fprintf(stderr, "osm2mysql: %v\nRun 'osm2mysql -h' for usage.\n", err)
		return ExitUsage
	}
	if opts.showVersion {
		fmt.Fprintf(stdout, "osm2mysql %s\n", version)
		return ExitOK
	}
	if err := runImport(opts); err != nil {
		fmt.Fprintf(stderr, "osm2mysql: %v\n", err)
		return ExitFailure
	}
	return ExitOK
}

func parseOptions(args []string) (options, *flag.FlagSet, error) {
	opts := options{
		dbHost:    "localhost",
		dbPort:    3306,
		dbUser:    "root",
		dbName:    "osm",
		tableName: poiTable,
		threads:   runtime.GOMAXPROCS(0),
	}
	fs := flag.NewFlagSet("osm2mysql", flag.ContinueOnError)
	// runCLI renders usage and parse errors so that -h can exit 0.
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.pbf, "pbf", "", "Path to the input PBF file (required)")
	fs.Var(&opts.selections, "tag", "OSM tag `name=value` to select; repeatable (required). Use `*` to match any non-empty value")
	fs.StringVar(&opts.dbHost, "db-host", opts.dbHost, "MySQL host")
	fs.IntVar(&opts.dbPort, "db-port", opts.dbPort, "MySQL port")
	fs.StringVar(&opts.dbUser, "db-user", opts.dbUser, "MySQL username")
	fs.StringVar(&opts.dbPass, "db-pass", opts.dbPass, "MySQL password")
	fs.StringVar(&opts.dbName, "db-name", opts.dbName, "MySQL database name")
	fs.StringVar(&opts.tableName, "table-name", opts.tableName, "MySQL output table name (recreated on each run)")
	fs.IntVar(&opts.threads, "threads", opts.threads, "Number of decoder workers (0 = Go runtime CPU parallelism)")
	fs.Int64Var(&opts.sortMem, "sort-mem", 0, "In-RAM budget (bytes) for the external sort (0 = auto: resource-aware)")
	fs.IntVar(&opts.centroidWorkers, "centroid-workers", 0, "Number of way-id ranges processed concurrently (0 = auto, resource-aware)")
	fs.BoolVar(&opts.showVersion, "version", false, "Print version and exit")

	if err := fs.Parse(args); err != nil {
		return options{}, fs, err
	}
	if opts.showVersion {
		return opts, fs, nil
	}
	if fs.NArg() > 0 {
		return options{}, fs, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if opts.pbf == "" {
		return options{}, fs, errors.New("missing required -pbf <file.osm.pbf>")
	}
	if len(opts.selections) == 0 {
		return options{}, fs, errors.New("missing required -tag <name=value>")
	}
	if err := validateTableName(opts.tableName); err != nil {
		return options{}, fs, err
	}
	if opts.dbPort < 1 || opts.dbPort > 65535 {
		return options{}, fs, fmt.Errorf("invalid -db-port %d: must be between 1 and 65535", opts.dbPort)
	}
	if opts.threads < 0 {
		return options{}, fs, fmt.Errorf("invalid -threads %d: must not be negative", opts.threads)
	}
	if opts.threads == 0 {
		opts.threads = runtime.GOMAXPROCS(0)
	}
	if opts.sortMem < 0 {
		return options{}, fs, fmt.Errorf("invalid -sort-mem %d: must not be negative", opts.sortMem)
	}
	if opts.centroidWorkers < 0 {
		return options{}, fs, fmt.Errorf("invalid -centroid-workers %d: must not be negative", opts.centroidWorkers)
	}
	return opts, fs, nil
}

func printUsage(w io.Writer, version string, fs *flag.FlagSet) {
	fmt.Fprintf(w, `osm2mysql %s: OpenStreetMap POI importer for MySQL

Usage:
  osm2mysql -pbf <file.osm.pbf> -tag <name=value> [-tag <name=value> ...] [options]

Examples:
  osm2mysql -pbf latvia.osm.pbf -tag tourism=viewpoint
  osm2mysql -pbf latvia.osm.pbf -tag historic=castle -tag "geological=*"
  osm2mysql -db-host db.example.com -db-user me -db-name osm -pbf region.osm.pbf -tag amenity=cafe

Options:
`, version)
	fs.SetOutput(w)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
	fmt.Fprint(w, `
Exit codes:
  0  import completed
  1  import failed (I/O, PBF or MySQL error)
  2  invalid command-line usage
`)
}

// runImport performs one import and returns a single, user-facing error.
func runImport(opts options) (err error) {
	pbfInfo, err := os.Stat(opts.pbf)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("PBF file not found: %s", opts.pbf)
		}
		return fmt.Errorf("cannot inspect PBF file %s: %w", opts.pbf, err)
	}
	if pbfInfo.IsDir() {
		return fmt.Errorf("PBF path is a directory: %s", opts.pbf)
	}
	if err := requireSortedPBF(opts.pbf); err != nil {
		return fmt.Errorf("invalid PBF: %w", err)
	}
	log := NewLogger()
	log.Log("Starting to parse: " + opts.pbf)

	ram := totalRAM()
	pbfBytes := uint64(pbfInfo.Size())

	// Honor explicit budgets; otherwise size the sort and centroid workers to
	// the machine and input file.
	effSortMem := opts.sortMem
	if effSortMem <= 0 {
		effSortMem = autoSortMem(ram, pbfBytes)
	}
	effCentroidWorkers := opts.centroidWorkers
	if effCentroidWorkers <= 0 {
		effCentroidWorkers = autoCentroidWorkers(runtime.NumCPU(), ram, pbfBytes)
	}
	startTime := time.Now()
	log.Log(fmt.Sprintf("Using %s decoder threads, %s centroid workers, %s sort memory",
		formatNumber(int64(opts.threads)), formatNumber(int64(effCentroidWorkers)), formatSize(uint64(effSortMem))))

	// Scratch/temp directory, next to the PBF.
	tempDir := filepath.Join(filepath.Dir(opts.pbf), ".osm2mysql")
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return fmt.Errorf("create scratch directory %s: %w", tempDir, err)
	}
	cleanup := func() {
		if rmErr := os.RemoveAll(tempDir); rmErr != nil {
			log.Log(fmt.Sprintf("WARNING: cannot remove temporary directory %s: %v", tempDir, rmErr))
		}
	}

	// Rough scratch estimate tuned to the bounded required-node pipeline. It is
	// intentionally conservative, but no longer assumes the previous ~5x worst
	// case from the old pipeline.
	estimate := estimateScratchBytes(pbfBytes, effSortMem)
	if free, err := freeDiskBytes(tempDir); err == nil && free < estimate {
		log.Log(fmt.Sprintf("WARNING: %s free in %s but ~%s of scratch may be needed for this run (estimate based on PBF size and pipeline settings). Free up space before running.",
			formatSize(free), tempDir, formatSize(estimate)))
	}

	imp := NewImporter(log, opts.dbHost, opts.dbPort, opts.dbUser, opts.dbPass, opts.dbName, opts.tableName)
	defer func() {
		if closeErr := imp.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("release import lock: %w", closeErr)
		}
	}()
	if err := imp.CreateDatabase(); err != nil {
		cleanup()
		return err
	}
	if err := imp.CreateTables(); err != nil {
		cleanup()
		return err
	}

	// Parse the selected nodes and areas.
	parser := NewParser(opts.pbf, tempDir, log, opts.threads, effSortMem, effCentroidWorkers)
	parser.selections = opts.selections
	if err := parser.Parse(); err != nil {
		cleanup()
		return err
	}

	if scratchBytes, err := directorySize(tempDir); err != nil {
		log.Log(fmt.Sprintf("WARNING: cannot measure temporary directory size after parsing: %v", err))
	} else {
		log.Log(fmt.Sprintf("Temporary directory size after parsing (not peak): %s", formatSize(scratchBytes)))
	}

	// Import CSVs into MySQL.
	if err := imp.Import(parser.PoiFiles()); err != nil {
		cleanup()
		return err
	}

	cleanup()
	log.Log("Temporary files removed")

	if err := imp.CreateIndexes(); err != nil {
		return err
	}

	log.Log(fmt.Sprintf("Finished in %s", formatDuration(time.Since(startTime))))
	return nil
}

func directorySize(path string) (uint64, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0, err
	}
	var total uint64
	for _, entry := range entries {
		entryPath := filepath.Join(path, entry.Name())
		if entry.IsDir() {
			size, err := directorySize(entryPath)
			if err != nil {
				return 0, err
			}
			total += size
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return 0, err
		}
		total += uint64(info.Size())
	}
	return total, nil
}

func estimateScratchBytes(pbfBytes uint64, sortMem int64) uint64 {
	// Required-node scratch is dominated by temporary refs/spills and centroids.
	// This estimate tracks sort pressure and PBF size, then adds headroom.
	sortBudget := uint64(1 << 30)
	if sortMem > 0 {
		sortBudget = uint64(sortMem)
	}
	base := (pbfBytes / 2) + (pbfBytes / 5) + (2 * sortBudget)
	estimate := (base * 5) / 4 // +25% headroom
	if estimate < pbfBytes {
		return pbfBytes
	}
	return estimate
}

// autoSortMem derives the external-sort budget from total RAM: ~12.5%,
// clamped to [512 MB, 8 GB] and capped by input size. The RAM fraction leaves
// room for decoder allocations and the OS page cache.
func autoSortMem(totalRAM, pbfBytes uint64) int64 {
	const maxMem = 8 << 30
	m := int64(2 << 30)
	if totalRAM > 0 {
		m = int64(totalRAM / 8)
	}
	if m < 1 {
		m = 1
	}
	if m > maxMem {
		m = maxMem
	}
	if pbfBytes > 0 && pbfBytes < uint64(m) {
		m = int64(pbfBytes)
	}
	return m
}

// autoCentroidWorkers scales way-range parallelism with input size and caps it
// by CPU, RAM, and an eight-worker ceiling.
func autoCentroidWorkers(cpus int, totalRAM, pbfBytes uint64) int {
	const (
		bytesPerWorker = 4 << 30
		bytesPerScale  = 2 << 30
		maxWorkers     = 8
	)
	if cpus < 1 {
		cpus = 1
	}
	ramLimit := 4
	if totalRAM > 0 {
		ramLimit = int((totalRAM + bytesPerWorker - 1) / bytesPerWorker)
		if ramLimit < 1 {
			ramLimit = 1
		}
	}
	sizeLimit := maxWorkers
	if pbfBytes > 0 {
		sizeLimit = int((pbfBytes + bytesPerScale - 1) / bytesPerScale)
		if sizeLimit < 1 {
			sizeLimit = 1
		}
	}
	workers := min(cpus, ramLimit, sizeLimit, maxWorkers)
	return max(workers, 1)
}
