package main

import (
	"fmt"

	"github.com/AnanmayS/f1-telemetry-go/internal/predict"
)

// picks lists, per predicted race, the model's favourite next to the pole
// sitter and the actual winner. It is a diagnostic for the backtest.
func picks(dataDir string, runs int) error {
	races, err := predict.LoadRaces(dataDir)
	if err != nil {
		return err
	}
	rep := predict.Backtest(races, runs)
	fmt.Println("| Race | Favourite (grid, win) | Pole sitter (win) | Winner (grid, win) |\n|---|---|---|---|")
	for _, r := range races {
		in := rep.Inputs[r.Key]
		if in == nil {
			continue
		}
		p := predict.Simulate(in, runs, uint64(r.Key))
		if len(p.Drivers) == 0 {
			continue
		}
		winner := 0
		for _, e := range r.Drivers {
			if e.Pos == 1 {
				winner = e.Number
			}
		}
		fav := p.Drivers[0]
		pole, won := "-", "-"
		for _, d := range p.Drivers {
			if d.Grid == 1 {
				pole = fmt.Sprintf("%s (%.0f%%)", d.Acronym, d.Win*100)
			}
			if d.Number == winner {
				won = fmt.Sprintf("%s (P%d, %.0f%%)", d.Acronym, d.Grid, d.Win*100)
			}
		}
		fmt.Printf("| %d %s | %s (P%d, %.0f%%) | %s | %s |\n", r.Year, r.Name, fav.Acronym, fav.Grid, fav.Win*100, pole, won)
	}
	return nil
}
