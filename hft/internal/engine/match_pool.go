package engine

type MatchPool struct {
	pool []Match
}

func NewMatchPool(capacity int) *MatchPool {
	return &MatchPool{
		pool: make([]Match, 0, capacity),
	}
}

// GetSlice returns a slice from the pool.
// We return a slice of matches to avoid allocating a new slice every time
// AddOrder tries to return matches.
func (p *MatchPool) GetSlice() []Match {
	if cap(p.pool) == 0 {
		return make([]Match, 0, 16)
	}

	s := p.pool
	p.pool = nil // taking it out
	s = s[:0]    // reset length
	return s
}

// PutSlice returns a slice back to the pool to be reused
func (p *MatchPool) PutSlice(s []Match) {
	// Simple pool keeping one slice
	p.pool = s
}
