package main

type limitQueue struct {
	limit int
	keys  []int
	store map[int]item
}

func (lm *limitQueue) add(i item) error {
	lm.keys = append(lm.keys, i.ID)
	if len(lm.store) >= lm.limit {
		lm.remove(lm.keys[0])
		lm.keys = lm.keys[1:]
	}
	lm.store[i.ID] = i
	return nil
}

func (lm *limitQueue) remove(id int) {
	delete(lm.store, id)
}
