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

## Backtest

Generated by `bench/predict-report.sh` on 2026-11-24. Do not edit below this line by hand; rerun the script.

Races in the record: 91 run (8 of them wet). The first 8 only train the model. 2000 simulated races per prediction.

| Races | Forecast | n | Top pick won | Probability given to the winner | Podium finishers in top-3 picks (of 3) | Brier win | Brier podium | Brier points | Mean position error |
|---|---|---|---|---|---|---|---|---|---|
| Holdout: 2025 onwards | model | 45 | 56% | 33% | 2.09 | 0.0299 | 0.0644 | 0.1555 | 2.59 |
| | grid only | 45 | 69% | 44% | 2.04 | 0.0248 | 0.0637 | 0.1648 | 2.90 |
| | simulation alone | 45 | 60% | 32% | 2.07 | 0.0308 | 0.0658 | 0.1559 | 2.57 |
| Tuning seasons: 2023-2024 | model | 38 | 61% | 32% | 1.92 | 0.0317 | 0.0731 | 0.1298 | 2.44 |
| | grid only | 38 | 50% | 34% | 1.89 | 0.0363 | 0.0774 | 0.1585 | 2.75 |
| 2023 | model | 14 | 79% | 40% | 1.79 | 0.0237 | 0.0769 | 0.1625 | 2.75 |
| | grid only | 14 | 57% | 38% | 1.79 | 0.0344 | 0.0853 | 0.1933 | 3.22 |
| 2024 | model | 24 | 50% | 28% | 2.00 | 0.0363 | 0.0709 | 0.1108 | 2.26 |
| | grid only | 24 | 46% | 32% | 1.96 | 0.0374 | 0.0728 | 0.1384 | 2.49 |
| 2025 | model | 24 | 46% | 33% | 2.25 | 0.0324 | 0.0611 | 0.1627 | 2.59 |
| | grid only | 24 | 67% | 42% | 2.25 | 0.0271 | 0.0598 | 0.1643 | 2.80 |
| 2026 | model | 21 | 67% | 33% | 1.90 | 0.0273 | 0.0679 | 0.1479 | 2.60 |
| | grid only | 21 | 71% | 46% | 1.81 | 0.0223 | 0.0678 | 0.1652 | 3.01 |

Likely range (10th to 90th percentile of finishing position, given the car finishes): 81% of classified drivers in the holdout finished inside it. A perfectly calibrated range would hold 80%.

### Calibration of podium probabilities (holdout)

| Predicted chance | Driver-races | Mean predicted | Actually on the podium |
|---|---|---|---|
| 0% to 5% | 621 | 1% | 1% |
| 5% to 20% | 74 | 12% | 8% |
| 20% to 50% | 107 | 35% | 31% |
| 50% to 80% | 115 | 65% | 70% |
| 80% to 100% | 11 | 82% | 82% |

"model" is the simulation blended with the finishing record of each grid slot; the blend for the newest race gives the grid-slot record 20%.

### Fitted values (model trained on every race)

- Tuned settings: pace uncertainty x1.00, start shuffle 0.2 s, pass threshold 1.3 s, grid gap 2.00 s, safety-car stop window 40% of the race.
- Race pace = -0.0061 + 0.11 x qualifying gap + 0.34 x driver form + 0.30 x team form + 0.0095 x grid slot (0 = pole, 1 = last). Pace, gap and form are fractions of a lap relative to the field median. Residual: 0.381% of a lap.
- SOFT: new-tyre pace -0.110% of a lap vs medium, wear 0.0486% of a lap per lap.
- MEDIUM: new-tyre pace +0.000% of a lap vs medium, wear 0.0436% of a lap per lap.
- HARD: new-tyre pace +0.059% of a lap vs medium, wear 0.0383% of a lap per lap.
- Lap-to-lap scatter: 0.60% of a lap.

## Race by race

The favourite, the pole sitter and the winner of every predicted race, with the win chance each was given.

| Race | Favourite (grid, win) | Pole sitter (win) | Winner (grid, win) |
|---|---|---|---|
| 2023 Austrian Grand Prix | VER (P1, 48%) | VER (48%) | VER (P1, 48%) |
| 2023 British Grand Prix | VER (P1, 51%) | VER (51%) | VER (P1, 51%) |
| 2023 Hungarian Grand Prix | HAM (P1, 35%) | HAM (35%) | VER (P2, 35%) |
| 2023 Belgian Grand Prix | LEC (P1, 30%) | LEC (30%) | VER (P6, 9%) |
| 2023 Dutch Grand Prix | VER (P1, 51%) | VER (51%) | VER (P1, 51%) |
| 2023 Italian Grand Prix | VER (P2, 37%) | SAI (25%) | VER (P2, 37%) |
| 2023 Singapore Grand Prix | SAI (P1, 33%) | SAI (33%) | SAI (P1, 33%) |
| 2023 Japanese Grand Prix | VER (P1, 48%) | VER (48%) | VER (P1, 48%) |
| 2023 Qatar Grand Prix | VER (P1, 52%) | VER (52%) | VER (P1, 52%) |
| 2023 United States Grand Prix | LEC (P1, 33%) | LEC (33%) | VER (P6, 10%) |
| 2023 Mexico City Grand Prix | VER (P3, 32%) | LEC (22%) | VER (P3, 32%) |
| 2023 São Paulo Grand Prix | VER (P1, 63%) | VER (63%) | VER (P1, 63%) |
| 2023 Las Vegas Grand Prix | VER (P2, 48%) | LEC (30%) | VER (P2, 48%) |
| 2023 Abu Dhabi Grand Prix | VER (P1, 48%) | VER (48%) | VER (P1, 48%) |
| 2024 Bahrain Grand Prix | VER (P1, 46%) | VER (46%) | VER (P1, 46%) |
| 2024 Saudi Arabian Grand Prix | VER (P1, 52%) | VER (52%) | VER (P1, 52%) |
| 2024 Australian Grand Prix | VER (P1, 51%) | VER (51%) | SAI (P2, 13%) |
| 2024 Japanese Grand Prix | VER (P1, 42%) | VER (42%) | VER (P1, 42%) |
| 2024 Chinese Grand Prix | VER (P1, 43%) | VER (43%) | VER (P1, 43%) |
| 2024 Miami Grand Prix | VER (P1, 48%) | VER (48%) | NOR (P5, 5%) |
| 2024 Emilia Romagna Grand Prix | VER (P1, 46%) | VER (46%) | VER (P1, 46%) |
| 2024 Monaco Grand Prix | LEC (P1, 33%) | LEC (33%) | LEC (P1, 33%) |
| 2024 Canadian Grand Prix | VER (P2, 38%) | RUS (21%) | VER (P2, 38%) |
| 2024 Spanish Grand Prix | VER (P2, 34%) | NOR (34%) | VER (P2, 34%) |
| 2024 Austrian Grand Prix | VER (P1, 38%) | VER (38%) | RUS (P3, 10%) |
| 2024 British Grand Prix | NOR (P3, 27%) | RUS (23%) | HAM (P2, 17%) |
| 2024 Hungarian Grand Prix | NOR (P1, 38%) | NOR (38%) | PIA (P2, 21%) |
| 2024 Belgian Grand Prix | LEC (P1, 27%) | LEC (27%) | HAM (P3, 13%) |
| 2024 Dutch Grand Prix | NOR (P1, 41%) | NOR (41%) | NOR (P1, 41%) |
| 2024 Italian Grand Prix | NOR (P1, 40%) | NOR (40%) | LEC (P4, 9%) |
| 2024 Azerbaijan Grand Prix | PIA (P2, 35%) | LEC (33%) | PIA (P2, 35%) |
| 2024 Singapore Grand Prix | NOR (P1, 43%) | NOR (43%) | NOR (P1, 43%) |
| 2024 United States Grand Prix | NOR (P1, 51%) | NOR (51%) | LEC (P4, 8%) |
| 2024 Mexico City Grand Prix | NOR (P3, 30%) | SAI (30%) | SAI (P1, 30%) |
| 2024 São Paulo Grand Prix | NOR (P1, 64%) | NOR (64%) | VER (P17, 0%) |
| 2024 Las Vegas Grand Prix | SAI (P2, 35%) | RUS (23%) | RUS (P1, 23%) |
| 2024 Qatar Grand Prix | RUS (P1, 32%) | RUS (32%) | VER (P2, 16%) |
| 2024 Abu Dhabi Grand Prix | NOR (P1, 43%) | NOR (43%) | NOR (P1, 43%) |
| 2025 Australian Grand Prix | NOR (P1, 51%) | NOR (51%) | NOR (P1, 51%) |
| 2025 Chinese Grand Prix | PIA (P1, 29%) | PIA (29%) | PIA (P1, 29%) |
| 2025 Japanese Grand Prix | NOR (P2, 34%) | VER (23%) | VER (P1, 23%) |
| 2025 Bahrain Grand Prix | PIA (P1, 40%) | PIA (40%) | PIA (P1, 40%) |
| 2025 Saudi Arabian Grand Prix | PIA (P2, 39%) | VER (24%) | PIA (P2, 39%) |
| 2025 Miami Grand Prix | NOR (P2, 35%) | VER (26%) | PIA (P4, 21%) |
| 2025 Emilia Romagna Grand Prix | PIA (P1, 50%) | PIA (50%) | VER (P2, 18%) |
| 2025 Monaco Grand Prix | NOR (P1, 50%) | NOR (50%) | NOR (P1, 50%) |
| 2025 Spanish Grand Prix | PIA (P1, 45%) | PIA (45%) | PIA (P1, 45%) |
| 2025 Canadian Grand Prix | PIA (P3, 46%) | RUS (5%) | RUS (P1, 5%) |
| 2025 Austrian Grand Prix | NOR (P1, 51%) | NOR (51%) | NOR (P1, 51%) |
| 2025 British Grand Prix | PIA (P2, 39%) | VER (24%) | NOR (P3, 27%) |
| 2025 Belgian Grand Prix | NOR (P1, 43%) | NOR (43%) | PIA (P2, 37%) |
| 2025 Hungarian Grand Prix | PIA (P2, 37%) | LEC (27%) | NOR (P3, 31%) |
| 2025 Dutch Grand Prix | PIA (P1, 49%) | PIA (49%) | PIA (P1, 49%) |
| 2025 Italian Grand Prix | NOR (P2, 44%) | VER (15%) | VER (P1, 15%) |
| 2025 Azerbaijan Grand Prix | VER (P1, 39%) | VER (39%) | VER (P1, 39%) |
| 2025 Singapore Grand Prix | PIA (P3, 35%) | RUS (22%) | RUS (P1, 22%) |
| 2025 United States Grand Prix | NOR (P2, 33%) | VER (29%) | VER (P1, 29%) |
| 2025 Mexico City Grand Prix | NOR (P1, 47%) | NOR (47%) | NOR (P1, 47%) |
| 2025 São Paulo Grand Prix | NOR (P1, 50%) | NOR (50%) | NOR (P1, 50%) |
| 2025 Las Vegas Grand Prix | NOR (P1, 49%) | NOR (49%) | VER (P2, 23%) |
| 2025 Qatar Grand Prix | PIA (P1, 42%) | PIA (42%) | VER (P3, 13%) |
| 2025 Abu Dhabi Grand Prix | NOR (P2, 34%) | VER (32%) | VER (P1, 32%) |
| 2026 Australian Grand Prix | RUS (P1, 35%) | RUS (35%) | RUS (P1, 35%) |
| 2026 Chinese Grand Prix | ANT (P1, 42%) | ANT (42%) | ANT (P1, 42%) |
| 2026 Japanese Grand Prix | ANT (P1, 39%) | ANT (39%) | ANT (P1, 39%) |
| 2026 Miami Grand Prix | ANT (P1, 48%) | ANT (48%) | ANT (P1, 48%) |
| 2026 Canadian Grand Prix | RUS (P1, 34%) | RUS (34%) | ANT (P2, 32%) |
| 2026 Monaco Grand Prix | ANT (P1, 51%) | ANT (51%) | ANT (P1, 51%) |
| 2026 Barcelona Grand Prix | RUS (P1, 34%) | RUS (34%) | HAM (P2, 23%) |
| 2026 Austrian Grand Prix | RUS (P1, 35%) | RUS (35%) | RUS (P1, 35%) |
| 2026 British Grand Prix | ANT (P1, 46%) | ANT (46%) | LEC (P2, 22%) |
| 2026 Belgian Grand Prix | ANT (P1, 50%) | ANT (50%) | ANT (P1, 50%) |
| 2026 Hungarian Grand Prix | LEC (P2, 28%) | NOR (26%) | NOR (P1, 26%) |
| 2026 Dutch Grand Prix | NOR (P1, 31%) | NOR (31%) | NOR (P1, 31%) |
| 2026 Italian Grand Prix | RUS (P2, 34%) | GAS (10%) | ANT (P19, 0%) |
| 2026 Spanish Grand Prix | NOR (P1, 31%) | NOR (31%) | ANT (P2, 30%) |
| 2026 Azerbaijan Grand Prix | RUS (P1, 38%) | RUS (38%) | RUS (P1, 38%) |
| 2026 Bahrain Grand Prix | VER (P1, 33%) | VER (33%) | VER (P1, 33%) |
| 2026 Singapore Grand Prix | VER (P1, 37%) | VER (37%) | VER (P1, 37%) |
| 2026 United States Grand Prix | ANT (P1, 36%) | ANT (36%) | ANT (P1, 36%) |
| 2026 Mexico City Grand Prix | HAM (P1, 40%) | HAM (40%) | VER (P5, 8%) |
| 2026 São Paulo Grand Prix | NOR (P1, 45%) | NOR (45%) | NOR (P1, 45%) |
| 2026 Las Vegas Grand Prix | VER (P1, 42%) | VER (42%) | VER (P1, 42%) |
