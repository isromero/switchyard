package pick

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/isromero/switchyard/internal/config"
)

// Slot is one job of the run that needs its own runner.
type Slot struct {
	Name string // key in the runners output
	Size string
}

var slotName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// maxSlots keeps a typo like test*500 from making hundreds of picks.
const maxSlots = 64

// ParseJobs reads a job list such as "check, test:medium*5". Items are
// separated by commas, spaces or newlines; each is name[:size][*count].
// A count of n gives the names name-1 ... name-n, for a matrix. Items
// without a size get defaultSize.
func ParseJobs(spec, defaultSize string) ([]Slot, error) {
	var out []Slot
	seen := map[string]bool{}
	add := func(name, size string) error {
		if seen[name] {
			return fmt.Errorf("job %q listed twice", name)
		}
		seen[name] = true
		out = append(out, Slot{Name: name, Size: size})
		return nil
	}
	for _, item := range strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '\r' }) {
		count := 0
		if i := strings.LastIndexByte(item, '*'); i >= 0 {
			n, err := strconv.Atoi(item[i+1:])
			if err != nil || n < 1 {
				return nil, fmt.Errorf("job %q: count must be a positive number", item)
			}
			count, item = n, item[:i]
		}
		name, size := item, defaultSize
		if i := strings.IndexByte(item, ':'); i >= 0 {
			name, size = item[:i], item[i+1:]
			if !validSize(size) {
				return nil, fmt.Errorf("job %q: unknown size %q", name, size)
			}
		}
		if !slotName.MatchString(name) {
			return nil, fmt.Errorf("job name %q: use letters, digits, '.', '_' or '-'", name)
		}
		if count == 0 {
			if err := add(name, size); err != nil {
				return nil, err
			}
		} else {
			for k := 1; k <= count; k++ {
				if err := add(fmt.Sprintf("%s-%d", name, k), size); err != nil {
					return nil, err
				}
			}
		}
		if len(out) > maxSlots {
			return nil, fmt.Errorf("more than %d jobs", maxSlots)
		}
	}
	return out, nil
}

func validSize(s string) bool {
	for _, v := range config.Sizes {
		if s == v {
			return true
		}
	}
	return false
}

// Assign picks a runner for each slot, in order. A self-hosted runner given
// to one slot is not offered to the next, so with two idle runners the
// first two slots get them and the rest go to hosted providers. Hosted
// providers run jobs side by side, so they can take any number of slots.
func Assign(cfg *config.Config, req Request, st State, slots []Slot) []Decision {
	claimed := map[string]int{}
	out := make([]Decision, len(slots))
	for i, s := range slots {
		r := req
		r.Size = s.Size
		r.Claimed = claimed
		d := Choose(cfg, r, st)
		if p := cfg.Provider(d.Provider); p != nil && p.Kind == config.KindSelfHosted {
			claimed[strings.ToLower(d.Label)]++
		}
		out[i] = d
	}
	return out
}

// claimedFor is how many slots of this pick already took a runner with
// the label.
func claimedFor(req Request, label string) int {
	return req.Claimed[strings.ToLower(label)]
}
