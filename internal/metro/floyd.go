// Package metro ports the reference C++ metro-station-priority algorithm
// (Floyd-Warshall over a graph of "real" and "fictive" per-station nodes,
// where the fictive node lets Floyd-Warshall charge a one-time boarding
// wait only when actually boarding a line) into Go, so a subscriber's
// custom priority stations can be factored into a re-ranked station list.
package metro

import (
	"sort"
	"strings"
	"sync"
)

const (
	inf = 100000.0
	eps = 1e-7
)

// fictive maps a real station index to its paired fictive node index in the
// combined 2*numStations graph, mirroring get_fictive() in the reference code.
func fictive(id int) int {
	return id + numStations
}

var (
	distOnce sync.Once
	distFlat []float64 // numNodes*numNodes, distFlat[i*numNodes+j] = shortest dist i->j
	numNodes = 2 * numStations
)

// buildAdjacency constructs the initial (non-transitive) adjacency matrix
// from stations/lineDuration/transfers/rails, matching init_station,
// add_transfer, and add_rail in the reference implementation. Parallel
// edges collapse to their minimum weight, matching floyd()'s min() when
// folding a node's edge list into the matrix.
func buildAdjacency() []float64 {
	n := numNodes
	adj := make([]float64, n*n)
	for i := range adj {
		adj[i] = inf
	}
	relax := func(from, to int, w float64) {
		idx := from*n + to
		if w < adj[idx] {
			adj[idx] = w
		}
	}

	for i := 0; i < numStations; i++ {
		// Boarding edge: waiting half the line's average interval,
		// integer-divided like the reference's `line_durations[lineId] / 2`.
		boarding := float64(lineDuration[stationLine[i]] / 2)
		relax(i, fictive(i), boarding)
		relax(fictive(i), i, boarding)
	}

	for _, t := range transfers {
		w := float64(t.duration)
		relax(t.from, t.to, w)
		relax(t.to, t.from, w)
		relax(fictive(t.from), t.to, w)
		relax(fictive(t.to), t.from, w)
	}

	for _, r := range rails {
		w := float64(r.duration)
		relax(fictive(r.from), fictive(r.to), w)
		relax(fictive(r.to), fictive(r.from), w)
	}

	return adj
}

// floydWarshall computes all-pairs shortest paths in place over adj (which
// must already hold direct-edge weights, inf elsewhere).
func floydWarshall(adj []float64) {
	n := numNodes
	for k := 0; k < n; k++ {
		rowK := adj[k*n : k*n+n]
		for i := 0; i < n; i++ {
			aik := adj[i*n+k]
			if aik+eps >= inf {
				continue
			}
			rowI := adj[i*n : i*n+n]
			for j := 0; j < n; j++ {
				akj := rowK[j]
				if akj+eps >= inf {
					continue
				}
				if v := aik + akj; v < rowI[j] {
					rowI[j] = v
				}
			}
		}
	}
}

// dists lazily computes (once per process) and caches the all-pairs
// shortest-path matrix, since it depends only on the static graph and is
// expensive (O(numNodes^3)) to (re)compute.
func dists() []float64 {
	distOnce.Do(func() {
		adj := buildAdjacency()
		floydWarshall(adj)
		distFlat = adj
	})
	return distFlat
}

// dist mirrors the reference dist(id1, id2): the shorter of going straight
// there or arriving via the destination's fictive (mid-ride) node.
func dist(d []float64, i, j int) float64 {
	n := numNodes
	direct := d[i*n+j]
	viaFictive := d[i*n+fictive(j)]
	if viaFictive < direct {
		return viaFictive
	}
	return direct
}

// normalizeStationName lowercases and folds ё/Ё to е/Е so station name
// comparisons ignore case and treat "е" and "ё" as equal, as requested.
func normalizeStationName(s string) string {
	s = strings.ReplaceAll(s, "ё", "е")
	s = strings.ReplaceAll(s, "Ё", "Е")
	return strings.ToLower(strings.TrimSpace(s))
}

// firstIndexByNormalizedName lazily builds a normalized-name -> first
// (lowest, i.e. earliest-declared) station index lookup, so that stations
// sharing a name across lines (e.g. "Белорусская") resolve to a single
// canonical index — matching "изменить вес только для первого совпадения".
var (
	nameIndexOnce sync.Once
	nameIndex     map[string]int
)

func firstIndexByNormalizedName() map[string]int {
	nameIndexOnce.Do(func() {
		nameIndex = make(map[string]int, numStations)
		for i := 0; i < numStations; i++ {
			key := normalizeStationName(stationName[i])
			if _, exists := nameIndex[key]; !exists {
				nameIndex[key] = i
			}
		}
	})
	return nameIndex
}

// MatchStationNames resolves each input name to the canonical display name
// of its first (lowest-index) matching station, ignoring case and treating
// "е"/"ё" as equal. Matched names are deduplicated, preserving input order.
// unmatched holds the input names that didn't match any known station.
func MatchStationNames(names []string) (matched []string, unmatched []string) {
	idx := firstIndexByNormalizedName()
	seen := make(map[int]bool)
	for _, raw := range names {
		key := normalizeStationName(raw)
		if key == "" {
			continue
		}
		i, ok := idx[key]
		if !ok {
			unmatched = append(unmatched, raw)
			continue
		}
		if !seen[i] {
			seen[i] = true
			matched = append(matched, stationName[i])
		}
	}
	return matched, unmatched
}

// StationScore is one ranked entry from TopStations.
type StationScore struct {
	Name  string
	Score float64
}

// TopStations ranks all stations by the reference get_dist_top_list
// algorithm with a single iteration (get_best_stations(numStations, 1) with
// all-ones initial weights), except each priority station (already resolved
// to canonical names, e.g. via MatchStationNames) has its weight set to
// numStations/2 before the ranking sum is computed, boosting stations close
// to it. Returns the top `count` stations, best (lowest weighted sum) first.
func TopStations(priorityNames []string, count int) []StationScore {
	d := dists()
	idx := firstIndexByNormalizedName()

	w := make([]float64, numStations)
	for i := range w {
		w[i] = 1
	}
	priorityWeight := float64(numStations) / 2
	for _, name := range priorityNames {
		if i, ok := idx[normalizeStationName(name)]; ok {
			w[i] = priorityWeight
		}
	}

	type ranked struct {
		sum float64
		idx int
	}
	ans := make([]ranked, numStations)
	for i := 0; i < numStations; i++ {
		var sum float64
		for j := 0; j < numStations; j++ {
			sum += dist(d, i, j) * w[j]
		}
		ans[i] = ranked{sum, i}
	}
	sort.Slice(ans, func(a, b int) bool {
		if ans[a].sum != ans[b].sum {
			return ans[a].sum < ans[b].sum
		}
		return ans[a].idx < ans[b].idx
	})

	if count > len(ans) {
		count = len(ans)
	}
	result := make([]StationScore, count)
	for i := 0; i < count; i++ {
		result[i] = StationScore{Name: stationName[ans[i].idx], Score: ans[i].sum}
	}
	return result
}
