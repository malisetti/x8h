package main

type item struct {
	ID      int    `json:"id"`
	Title   string `json:"title"`
	URL     string `json:"url"`
	Deleted bool   `json:"deleted"`
	Dead    bool   `json:"dead"`

	From     string `json:"from"`
	Priority int    `json:"priority"`

	Added    unixTime `json:"added"`
	RemoveAt unixTime `json:"removeAt"`
	Domain   string   `json:"domain"`
}
