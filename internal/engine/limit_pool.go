package engine

// Pool for Limit objects
type LimitPool struct {
	pool []*Limit
}

func NewLimitPool(capacity int) *LimitPool {
	return &LimitPool{
		pool: make([]*Limit, 0, capacity),
	}
}

func (p *LimitPool) Get(price uint64) *Limit {
	if len(p.pool) == 0 {
		return NewLimit(price)
	}
	// pop
	l := p.pool[len(p.pool)-1]
	p.pool = p.pool[:len(p.pool)-1]

	// reset
	l.Price = price
	l.TotalVolume = 0
	l.Head = nil
	l.Tail = nil

	return l
}

func (p *LimitPool) Put(l *Limit) {
	p.pool = append(p.pool, l)
}
