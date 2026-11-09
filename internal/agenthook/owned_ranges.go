package agenthook

import (
	"cmp"
	"context"
	"math/big"
	"strings"

	roborevgit "go.kenn.io/roborev/internal/git"
)

// rangeEndpoint peels ref to a commit, except the empty tree that enqueue
// stores as the start of a range beginning at the root commit. That start has
// no ancestry, so it resolves as itself and never enters the graph walk.
func rangeEndpoint(ref string) string {
	if ref == roborevgit.EmptyTreeSHA {
		return ref
	}
	return ref + "^{commit}"
}

// ownedRanges checks all ranges with one parent-first graph walk. Each node
// carries its owned ancestors, so subtracting endpoint sets implements two-dot
// ranges and their symmetric difference implements three-dot ranges.
func ownedRanges(ctx context.Context, root string, refs []string, owned map[string]bool) map[string]bool {
	ends := map[string][2]string{}
	names := []string{}
	for _, ref := range refs {
		if !roborevgit.IsRange(ref) {
			continue
		}
		if _, seen := ends[ref]; seen {
			continue
		}
		start, end, symmetric := strings.Cut(ref, "...")
		if !symmetric {
			start, end, _ = roborevgit.ParseRange(ref)
		}
		pair := [2]string{rangeEndpoint(cmp.Or(start, "HEAD")), rangeEndpoint(cmp.Or(end, "HEAD"))}
		ends[ref] = pair
		names = append(names, pair[0], pair[1])
	}
	if len(ends) == 0 {
		return nil
	}
	resolved, err := roborevgit.ResolveSHAsCtx(ctx, root, names)
	if err != nil {
		return nil
	}
	input := strings.Builder{}
	for _, sha := range resolved {
		if sha != "" && sha != roborevgit.EmptyTreeSHA {
			input.WriteString(sha + "\n")
		}
	}
	indices := map[string]int{}
	args := []string{"merge-base", "--octopus"}
	for sha, isOwned := range owned {
		if isOwned {
			args = append(args, sha)
			indices[sha] = len(indices)
		}
	}
	// No owned commit precedes their common ancestor. Exclude its parents so
	// old shared history contributes no work or bits to the graph below.
	if base, err := agentHookGit.Output(ctx, root, args...); err == nil {
		input.WriteString("^" + strings.TrimSpace(string(base)) + "^@\n")
	}
	out, _, err := agentHookGit.Run(ctx, root, strings.NewReader(input.String()), "rev-list", "--reverse", "--topo-order", "--parents", "--stdin")
	if err != nil {
		return nil
	}
	ancestors := map[string]*big.Int{}
	for line := range strings.SplitSeq(string(out), "\n") {
		if ctx.Err() != nil {
			return nil
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		bits := new(big.Int)
		if index, ok := indices[fields[0]]; ok {
			bits.SetBit(bits, index, 1)
		}
		for _, parent := range fields[1:] {
			if bitsOfParent := ancestors[parent]; bitsOfParent != nil {
				bits.Or(bits, bitsOfParent)
			}
		}
		if bits.Sign() != 0 {
			ancestors[fields[0]] = bits
		}
	}
	matches := map[string]bool{}
	for ref, pair := range ends {
		if resolved[pair[0]] == "" || resolved[pair[1]] == "" {
			continue
		}
		bits := new(big.Int)
		if end := ancestors[resolved[pair[1]]]; end != nil {
			bits.Set(end)
		}
		if start := ancestors[resolved[pair[0]]]; start != nil {
			if strings.Contains(ref, "...") {
				bits.Xor(bits, start)
			} else {
				bits.AndNot(bits, start)
			}
		}
		matches[ref] = bits.Sign() != 0
	}
	return matches
}
