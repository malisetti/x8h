package main

type limitQueue struct {
	Limit int           `json:"limit"`
	Keys  []int         `json:"keys"`
	Store map[int]*item `json:"store"`
}

func (lm *limitQueue) add(i *item) *item {
	lm.Store[i.ID] = i
	lm.Keys = append(lm.Keys, i.ID)

	if len(lm.Store) >= lm.Limit {
		it := lm.remove(lm.Keys[0])
		lm.Keys = lm.Keys[1:]

		return it
	}

	return nil
}

func (lm *limitQueue) remove(id int) *item {
	i := lm.Store[id]
	delete(lm.Store, id)
	return i
}
