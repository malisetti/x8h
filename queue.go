package main

type limitQueue struct {
	Limit int           `json:"limit"`
	Keys  []int         `json:"keys"`
	Store map[int]*item `json:"store"`
}

func (lm *limitQueue) add(i *item) (replaced bool, removedItemIfAny *item) {
	for pos, id := range lm.Keys {
		if id == i.ID {
			lm.Keys = append(lm.Keys[:pos], lm.Keys[pos+1:]...)
			break
		}
	}

	lm.Keys = append(lm.Keys, i.ID)
	if eit, ok := lm.Store[i.ID]; ok {
		i.TweetID = eit.TweetID
		replaced = true
	}
	lm.Store[i.ID] = i

	if len(lm.Store) >= lm.Limit {
		removedItemIfAny = lm.remove(lm.Keys[0])
		lm.Keys = lm.Keys[1:]

		return
	}

	return
}

func (lm *limitQueue) remove(id int) *item {
	i, ok := lm.Store[id]
	if !ok {
		return nil
	}
	delete(lm.Store, id)
	return i
}
