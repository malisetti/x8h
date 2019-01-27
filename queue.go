package main

type limitQueue struct {
	limit int
	keys  []int
	store map[int]*item
}

func (lm *limitQueue) add(i *item) *item {
	lm.store[i.ID] = i
	lm.keys = append(lm.keys, i.ID)

	if len(lm.store) >= lm.limit {
		it := lm.remove(lm.keys[0])
		lm.keys = lm.keys[1:]

		return it
	}

	return nil
}

func (lm *limitQueue) remove(id int) *item {
	i := lm.store[id]
	delete(lm.store, id)
	return i
}
