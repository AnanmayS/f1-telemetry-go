# Race predictions

ghostlap estimates the outcome of a Grand Prix by simulating it 10,000 times. This page says how, and how well it has done on races it had not seen. The first part is written by hand (`docs/predictions-head.md`); everything from "Backtest" down is generated.

This is a change of scope: the original spec listed predictions as a non-goal. It is an estimate from public timing data, shown for interest. It is not betting odds or advice.

## Summary of what the backtest shows

- **Finishing order and points:** the model beats "everyone finishes where they start". Its mean position error is lower and its points probabilities are sharper.
- **Likely range:** well calibrated. About 80% of finishers end inside the 10th-90th percentile range, as they should.
- **Podium:** about level with the grid-only baseline.
- **Winner:** worse than the grid-only baseline on 2025 onwards. In those seasons the pole sitter won about two races in three, and the model too often preferred a car with better recent form starting second or third. If all you want is the winner, back the pole sitter.

## What is predicted

Each driver's chance to win, finish on the podium, score points (top ten) and retire, plus the range of finishing positions. Sprints are not predicted. A race appears once its qualifying result exists; before the race is run the grid is the provisional qualifying order, without penalties.

## Data

One record per Grand Prix, built by `predict fetch` from OpenF1 (see [DATA.md](DATA.md)): grid, qualifying times per segment, result, every lap time, tyre stints, and safety car, virtual safety car and red flag periods from race control messages. Stored in `data/races.json.gz`.

## Model

Everything is fitted only on races before the one being predicted.

1. **Clean laps.** Laps that are not lap 1, an in-lap, an out-lap, under or just after a neutralisation, on wet tyres, or more than 7% slower than the race's median lap. Wet races are not used for fitting.
2. **Tyres and fuel.** One least-squares regression over all clean laps, with a fixed effect per driver and race: lap time against race distance covered (fuel burn and track evolution), compound, and tyre age per compound.
3. **Race pace.** A driver's pace in a race is the median of their clean laps after removing the tyre and fuel terms, relative to the field median.
4. **Expected pace for the next race.** A regression of race pace on four things known beforehand: the qualifying gap (to the best time of the last segment the driver ran in), the driver's recent race pace (followed by driver, not car number), the team's recent race pace, and the grid slot. "Recent" is the last 8 races, weighted 0.7 per race back. Front-half grid slots get five times the weight. The spread of the residuals is the pace uncertainty.
5. **Measured from past races, not assumed:** pit stop time loss per circuit (in-lap plus out-lap against two clean laps), the cheaper cost of a stop under a safety car, retirement rate and when retirements happen, strategies (the stint plans finishers actually used at that circuit), and safety car patterns (a past race's neutralisations are replayed; the same circuit counts three times).

## Simulation

Each run draws a pace for every driver, a strategy, retirements and a safety car pattern, then steps lap by lap. Lap time is the reference lap times (1 + pace + compound offset + wear x tyre age + noise). A car that catches another must pass it: the chance per lap rises with its lap-time advantage; otherwise it sits 0.4 s behind. Under a safety car the field bunches up and drivers with a stop due soon take it early at reduced cost.

Four settings are not measured directly and are tuned on 2023-2024 only: the gap between cars after lap 1, the size of the start shuffle, the lap-time advantage needed to pass, and how early a stop is brought forward under a safety car. A fifth, a multiplier on pace uncertainty, is fixed at 1 (the measured value); tuning it on 2023-2024 made the model overconfident in later seasons.

Finally the simulated position chances are blended with how each grid slot has finished historically. The blend weight for a race is the one that would have scored best on the races predicted before it.

## Caveats on the backtest itself

- The pace-uncertainty multiplier was fixed at 1 after seeing that tuning it hurt the holdout seasons. That is one design decision the holdout influenced, so the holdout is not perfectly untouched.
- The backtest knows a little more than a live prediction would: it uses the grid as it actually formed up (penalties included), the real race distance, and it leaves out drivers who did not start. A prediction made before the race uses the provisional qualifying order and last year's distance.
- When the tuned gap between cars or safety-car window sits at the edge of its search range (see "Tuned settings" below), the tuning has not settled and a wider search could change it.
- Strategies and pit loss come from the same circuit when there are at least 12 past strategies or 5 past green-flag stops there, and from all circuits otherwise.
- The likely finishing range comes from the simulation alone; the win, podium and points chances are from the blend with grid-slot history.
- A car that passes another is not also checked against every car it then catches on the same lap.

## What it does not know

Weather, grid penalties applied after qualifying, upgrades, team orders, damage, and anything that happens in the race. Tyre wear is a straight line. Overtaking difficulty is the same at every circuit. Monaco and Monza are treated alike apart from pit loss, strategies and safety car history.

## Reading the backtest

The model predicts every race from the ninth onwards using only earlier races. "Grid only" is the baseline: each grid slot's historical win, podium and points frequency, and "finishes where it starts" for position. Brier score is the mean squared error of a probability, per driver; lower is better. Seasons up to 2024 were used to tune the settings, so the honest numbers are the holdout rows.
