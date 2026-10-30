# Calibration

`gdiskmap calibrate` builds the bay label config by lighting one slot's
identify LED at a time and asking which front-panel bay is lit. It infers
the numbering pattern so that most slots need only a confirmation, or no
prompt at all.

## Running it

```sh
gdiskmap calibrate                 # every controller
gdiskmap calibrate c1              # one controller, by name
gdiskmap calibrate 0000:01:00.0    # ... by PCI address
gdiskmap calibrate 0x500605b0071f24b0   # ... by enclosure ID
```

Flags:

| Flag | Default | Description |
|---|---|---|
| `-group-size N` | 4 | Number of consecutive slots that share a numbering pattern, normally the lanes of one connector (SFF-8087/SFF-8643 carry 4). |
| `-confirm-all` | off | Confirm every predicted bay instead of one per group. |

At each prompt, answer with:

| Input | Meaning |
|---|---|
| a number | The bay number of the lit slot (or the correct number when a prediction is wrong). |
| Enter, `y` | Accept a predicted bay number. |
| `n` | Reject a prediction; you are then asked for the correct number. |
| `s` | Skip: no lit bay is visible. |
| `q`, end of input | Stop, keeping the bays mapped so far. |

Bay numbers must be non-negative integers and unique across the whole
config. Text labels such as `A3` can be set by editing the config afterwards.

When calibration finishes, the result is printed and you are asked before
anything is written. Saving replaces all existing labels of the calibrated
enclosures and keeps a timestamped backup of the previous file
(`/etc/gdiskmap.toml.<timestamp>.bak`). Labels of other enclosures are kept,
and their bay numbers cannot be reused.

Interrupting with Ctrl-C turns the lit LED off and saves nothing. The
identify LED of every calibrated slot is off when calibration ends, even if
it was on before.

## How the pattern is inferred

Slots are split into groups of `-group-size` consecutive slots. Within a
group, bay numbers are assumed to run in a straight line: the i-th slot is
in bay `base + step × i`, where `step` is +1 or −1.

1. For the first group, gdiskmap asks about slots until it has two answers.
   They determine `base` and `step`.
2. It then lights the last unanswered slot of the group and asks you to
   confirm the predicted bay. If you accept, the remaining slots of the group
   are filled in from the pattern.
3. Each following group, including the first group of the next controller,
   is predicted to continue the previous group's numbering in the same
   direction: if the previous group used bays 0–3, the next is predicted to
   use 4–7. One slot is lit for confirmation.
4. A rejected prediction, or answers that do not form a straight line, fall
   back to asking about the rest of that group slot by slot. The next group
   learns its pattern afresh.

A skipped slot is filled in from a confirmed pattern. This lets empty bays
whose LEDs do not light still receive a label.

On a controller with eight slots in two reversed connectors (the common
case), calibration takes four prompts: two answers and a confirmation for the
first connector, and one confirmation for the second. Each further controller
numbered continuously takes two.

The confirmation checks one predicted slot per group. A group whose numbering
differs from the prediction only in slots that are not shown would be
mislabelled; use `-confirm-all` if your backplane numbering is irregular, or
check the result with `gdiskmap locate on <label>`.
