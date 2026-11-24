package main

import (
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/AnanmayS/f1-telemetry-go/internal/predict"
)

func backtest(dataDir string, runs int) error {
	races, err := predict.LoadRaces(dataDir)
	if err != nil {
		return err
	}
	rep := predict.Backtest(races, runs)
	report(races, rep, runs)
	return nil
}

func build(dataDir string, runs int) error {
	races, err := predict.LoadRaces(dataDir)
	if err != nil {
		return err
	}
	rep := predict.Backtest(races, runs)
	f := predict.BuildFile(races, rep, time.Now())
	if err := predict.SaveFile(dataDir, f); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s: %d races\n", predict.PredictionsFile, len(f.Races))
	return nil
}

func pct(v float64) string { return fmt.Sprintf("%.0f%%", v*100) }

func report(races []*predict.Race, rep *predict.Report, runs int) {
	var run, wet int
	for _, r := range races {
		if r.Run {
			run++
			if r.Wet() {
				wet++
			}
		}
	}
	row := func(name string, m, b predict.Metrics) {
		fmt.Printf("| %s | model | %d | %s | %s | %.2f | %.4f | %.4f | %.4f | %.2f |\n", name, m.Races, pct(m.WinnerHit), pct(m.WinnerProb), m.PodiumOverlap, m.BrierWin, m.BrierPodium, m.BrierPoints, m.MAE)
		fmt.Printf("| | grid only | %d | %s | %s | %.2f | %.4f | %.4f | %.4f | %.2f |\n", b.Races, pct(b.WinnerHit), pct(b.WinnerProb), b.PodiumOverlap, b.BrierWin, b.BrierPodium, b.BrierPoints, b.MAE)
	}
	fmt.Printf("Races in the record: %d run (%d of them wet). The first %d only train the model. %d simulated races per prediction.\n\n", run, wet, predict.MinTrain, runs)
	fmt.Println("| Races | Forecast | n | Top pick won | Probability given to the winner | Podium finishers in top-3 picks (of 3) | Brier win | Brier podium | Brier points | Mean position error |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|---|")
	row("Holdout: 2025 onwards", rep.Holdout, rep.HoldoutBase)
	sm := rep.HoldoutSim
	fmt.Printf("| | simulation alone | %d | %s | %s | %.2f | %.4f | %.4f | %.4f | %.2f |\n", sm.Races, pct(sm.WinnerHit), pct(sm.WinnerProb), sm.PodiumOverlap, sm.BrierWin, sm.BrierPodium, sm.BrierPoints, sm.MAE)
	row("Tuning seasons: "+rep.TuneYears, rep.Tune, rep.TuneBase)
	var years []int
	for y := range rep.ByYear {
		years = append(years, y)
	}
	sort.Ints(years)
	for _, y := range years {
		row(fmt.Sprint(y), rep.ByYear[y][0], rep.ByYear[y][1])
	}
	fmt.Printf("\nLikely range (10th to 90th percentile of finishing position, given the car finishes): %s of classified drivers in the holdout finished inside it. A perfectly calibrated range would hold 80%%.\n", pct(rep.Holdout.Coverage))
	fmt.Println("\n### Calibration of podium probabilities (holdout)")
	fmt.Println("\n| Predicted chance | Driver-races | Mean predicted | Actually on the podium |\n|---|---|---|---|")
	for _, b := range rep.Calibration {
		fmt.Printf("| %s to %s | %d | %s | %s |\n", pct(b.Lo), pct(b.Hi), b.N, pct(b.Predicted), pct(b.Observed))
	}
	p := rep.Params
	fmt.Printf("\n\"model\" is the simulation blended with the finishing record of each grid slot; the blend for the newest race gives the grid-slot record %.0f%%.\n", rep.PriorWeight*100)
	fmt.Println("\n### Fitted values (model trained on every race)")
	fmt.Printf("\n- Tuned settings: pace uncertainty x%.2f, start shuffle %.1f s, pass threshold %.1f s, grid gap %.2f s, safety-car stop window %.0f%% of the race.\n", p.PaceScale, p.StartSD, p.PassDelta, p.GridGap, p.SCWindow*100)
	if len(rep.Coef) == 5 {
		fmt.Printf("- Race pace = %.4f + %.2f x qualifying gap + %.2f x driver form + %.2f x team form + %.4f x grid slot (0 = pole, 1 = last). Pace, gap and form are fractions of a lap relative to the field median. Residual: %.3f%% of a lap.\n", rep.Coef[0], rep.Coef[1], rep.Coef[2], rep.Coef[3], rep.Coef[4], rep.PaceSD*100)
	}
	for _, c := range []string{"SOFT", "MEDIUM", "HARD"} {
		t := rep.Tyres[c]
		fmt.Printf("- %s: new-tyre pace %+.3f%% of a lap vs medium, wear %.4f%% of a lap per lap.\n", c, t.Offset*100, t.Slope*100)
	}
	fmt.Printf("- Lap-to-lap scatter: %.2f%% of a lap.\n", rep.LapSD*100)
}
