package main

type limitQueue struct {
	Limit int           `json:"limit"`
	Keys  []int         `json:"keys"`
	Store map[int]*item `json:"store"`
}

func (lm *limitQueue) add(i *item) *item {
	for pos, id := range lm.Keys {
		if id == i.ID {
			lm.Keys = append(lm.Keys[:pos], lm.Keys[pos+1:]...)
			break
		}
	}

	lm.Keys = append(lm.Keys, i.ID)
	lm.Store[i.ID] = i

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
