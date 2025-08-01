package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pterm/pterm"
)

var fHost = flag.String("host", "127.0.0.1:6379", "Redis host")
var fCompareHost = flag.String("compare-host", "", "Redis host to compare with")
var fClientBuffer = flag.Int("buffer", 100, "How many records to buffer per client")
var fPace = flag.Bool("pace", true, "whether to pace the traffic according to the original timings.false - to pace as fast as possible")
var fSkip = flag.Uint("skip", 0, "skip N records")
var fSkipTimeSec = flag.Int("skip-time-sec", 0, "skip records in the first N seconds of the recording")
var fIgnoreParseErrors = flag.Bool("ignore-parse-errors", false, "ignore parsing errors")
var fTimeLimit = flag.Int("time-limit", 0, "time limit in seconds (0 = no limit)")

func RenderTable(area *pterm.AreaPrinter, files []string, workers []FileWorker) {
	tableData := pterm.TableData{{"file", "parsed", "processed", "delayed", "clients", "avg(us)", "p75(us)", "p90(us)", "p99(us)", "p99.9(us)"}}
	for i := range workers {
		workers[i].latencyMu.Lock()
		avg := 0.0
		if workers[i].latencyCount > 0 {
			avg = workers[i].latencySum / float64(workers[i].latencyCount)
		}
		p75 := workers[i].latencyDigest.Quantile(0.75)
		p90 := workers[i].latencyDigest.Quantile(0.9)
		p99 := workers[i].latencyDigest.Quantile(0.99)
		p999 := workers[i].latencyDigest.Quantile(0.999)
		workers[i].latencyMu.Unlock()
		tableData = append(tableData, []string{
			files[i],
			fmt.Sprint(atomic.LoadUint64(&workers[i].parsed)),
			fmt.Sprint(atomic.LoadUint64(&workers[i].processed)),
			fmt.Sprint(atomic.LoadUint64(&workers[i].delayed)),
			fmt.Sprint(atomic.LoadUint64(&workers[i].clients)),
			fmt.Sprintf("%.0f", avg),
			fmt.Sprintf("%.0f", p75),
			fmt.Sprintf("%.0f", p90),
			fmt.Sprintf("%.0f", p99),
			fmt.Sprintf("%.0f", p999),
		})
	}
	content, _ := pterm.DefaultTable.WithHasHeader().WithBoxed().WithData(tableData).Srender()
	area.Update(content)
}

// RenderPipelineRangesTable renders the latency digests for each pipeline range
func RenderPipelineRangesTable(area *pterm.AreaPrinter, files []string, workers []FileWorker) {
	tableData := pterm.TableData{{"file", "Pipeline Range", "p75(us)", "p90(us)", "p99(us)", "p99.9(us)"}}
	for i := range workers {
		workers[i].latencyMu.Lock()
		for _, rng := range pipelineRanges {
			if digest, ok := workers[i].perRange[rng.label]; ok {
				p75 := digest.Quantile(0.75)
				p90 := digest.Quantile(0.9)
				p99 := digest.Quantile(0.99)
				p999 := digest.Quantile(0.999)
				tableData = append(tableData, []string{
					files[i],
					rng.label,
					fmt.Sprintf("%.0f", p75),
					fmt.Sprintf("%.0f", p90),
					fmt.Sprintf("%.0f", p99),
					fmt.Sprintf("%.0f", p999),
				})
			}
		}
		workers[i].latencyMu.Unlock()
	}
	content, _ := pterm.DefaultTable.WithHasHeader().WithBoxed().WithData(tableData).Srender()
	area.Update(content)
}

func Run(files []string) {
	baseTime := DetermineBaseTime(files)

	var skipUntil uint64
	effectiveBaseTime := baseTime
	if *fSkipTimeSec > 0 {
		skipDuration := time.Duration(*fSkipTimeSec) * time.Second
		skipUntil = uint64(baseTime.Add(skipDuration).UnixNano())
		effectiveBaseTime = baseTime.Add(skipDuration)
	}
	timeOffset := time.Now().Add(500 * time.Millisecond).Sub(effectiveBaseTime)
	fmt.Println("Offset -> ", timeOffset)

	// Calculate stop time based on recording timestamps if time limit is specified
	var stopUntil uint64
	if *fTimeLimit > 0 {
		limitDuration := time.Duration(*fTimeLimit) * time.Second
		stopUntil = uint64(effectiveBaseTime.Add(limitDuration).UnixNano())
		fmt.Printf("Time limit set to %d seconds\n", *fTimeLimit)
	}

	// Start a worker for every file. They take care of spawning client workers.
	var wg sync.WaitGroup
	workers := make([]FileWorker, len(files))
	for i := range workers {
		workers[i] = FileWorker{timeOffset: timeOffset, skipUntil: skipUntil, stopUntil: stopUntil}
		wg.Add(1)
		go workers[i].Run(files[i], &wg)
	}

	wgDone := make(chan bool)
	go func() {
		wg.Wait()
		wgDone <- true
	}()

	// Render table while running
	area, _ := pterm.DefaultArea.WithCenter().Start()
	for running := true; running; {
		select {
		case <-wgDone:
			running = false
		case <-time.After(100 * time.Millisecond):
			RenderTable(area, files, workers)
		}
	}

	RenderTable(area, files, workers) // to show last stats
	areaPipelineRanges, _ := pterm.DefaultArea.WithCenter().Start()
	RenderPipelineRangesTable(areaPipelineRanges, files, workers) // to render per pipeline-range latency digests
}

func Print(files []string) {
	type StreamTop struct {
		record Record
		ch     chan Record
	}

	// Start file reader goroutines
	var wg sync.WaitGroup
	wg.Add(len(files))

	tops := make([]StreamTop, len(files))
	for i, file := range files {
		tops[i].ch = make(chan Record, 100)
		go func(ch chan Record, file string) {
			parseRecords(file, func(r Record) bool {
				ch <- r
				return true
			}, *fIgnoreParseErrors)
			close(ch)
			wg.Done()
		}(tops[i].ch, file)
	}

	// Pick record with minimum time from each channel
	for {
		minTime := ^uint64(0)
		minIndex := -1
		for i := range tops {
			if tops[i].record.Time == 0 {
				if r, ok := <-tops[i].ch; ok {
					tops[i].record = r
				}
			}

			if rt := tops[i].record.Time; rt > 0 && rt < minTime {
				minTime = rt
				minIndex = i
			}
		}

		if minIndex == -1 {
			break
		}

		fmt.Println(tops[minIndex].record.values...)
		tops[minIndex].record = Record{}
	}

	wg.Wait()
}

func Analyze(files []string) {
	total := 0
	chained := 0
	clients := 0
	cmdCounts := make(map[string]uint)

	// count stats
	for _, file := range files {
		fileClients := make(map[uint32]bool)

		parseRecords(file, func(r Record) bool {
			total += 1
			if r.HasMore > 0 {
				chained += 1
			}

			fileClients[r.Client] = true
			cmdCounts[r.values[0].(string)] += 1

			return true
		}, *fIgnoreParseErrors)

		clients += len(fileClients)
	}

	// sort commands by frequencies
	type Freq struct {
		cmd   string
		count uint
	}
	var sortedCmds []Freq
	for cmd, count := range cmdCounts {
		sortedCmds = append(sortedCmds, Freq{cmd, count})
	}
	sort.Slice(sortedCmds, func(i, j int) bool {
		return sortedCmds[i].count > sortedCmds[j].count
	})

	// Print all the info
	fmt.Println("Total commands", total)
	fmt.Println("Has more%", 100*float32(chained)/float32(total))
	fmt.Println("Total clients", clients)

	for _, freq := range sortedCmds {
		fmt.Printf("%8d | %v \n", freq.count, freq.cmd)
	}
}

func main() {
	flag.Usage = func() {
		binaryName := os.Args[0]

		fmt.Fprintf(os.Stderr, "Usage: %s [options] <command> <files...>\n", binaryName)
		fmt.Fprintln(os.Stderr, "\nOptions:")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, "\nCommands:")
		fmt.Fprintln(os.Stderr, "  run  - replays the traffic")
		fmt.Fprintln(os.Stderr, "  print - prints the command")
		fmt.Fprintln(os.Stderr, "  analyze - analyzes the traffic")

		fmt.Fprintln(os.Stderr, "\nExamples:")
		fmt.Fprintf(os.Stderr, "   %s -host 192.168.1.10:6379 -buffer 50 run *.bin\n", binaryName)
		fmt.Fprintf(os.Stderr, "   %s -skip-time-sec 30 run *.bin\n", binaryName)
		fmt.Fprintf(os.Stderr, "   %s -time-limit 60 run *.bin\n", binaryName)
		fmt.Fprintf(os.Stderr, "   %s print *.bin\n", binaryName)
	}

	flag.Parse()
	if flag.NArg() < 2 {
		flag.Usage()
		os.Exit(1)
	}

	cmd := flag.Arg(0)
	files := flag.Args()[1:]

	switch strings.ToLower(cmd) {
	case "run":
		Run(files)
	case "print":
		Print(files)
	case "analyze":
		Analyze(files)
	}
}
