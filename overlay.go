package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
	"time"
)

const listSessionTTL = 10 * time.Minute

// listEntry is one item of a listing: either an object or a common prefix.
type listEntry struct {
	key    string
	obj    *ObjectMeta
	prefix bool
}

// cursorState is the resume position of one backend's listing stream: the
// backend continuation token plus the entries already fetched from that
// backend but not yet consumed by the merge. Persisting the unconsumed
// tail across pages is what keeps the k-way merge from skipping entries.
type cursorState struct {
	after     string
	remaining []listEntry
	done      bool
	missing   bool // backend answered ErrNotFound: it does not hold the bucket
}

// listSession holds the resume state of both backends for one paginated
// listing. Client-facing next tokens are short session IDs mapping here.
type listSession struct {
	overlay  cursorState
	baseline cursorState
	expires  time.Time
}

type listSessions struct {
	mu     sync.Mutex
	nextID uint64
	active map[uint64]listSession
}

func newListSessions() *listSessions {
	return &listSessions{active: map[uint64]listSession{}}
}

func (ls *listSessions) create(ov, bs cursorState) string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.gcLocked()
	ls.nextID++
	ls.active[ls.nextID] = listSession{
		overlay: ov, baseline: bs,
		expires: time.Now().Add(listSessionTTL),
	}
	return strconv.FormatUint(ls.nextID, 10)
}

func (ls *listSessions) lookup(token string) (ov, bs cursorState) {
	if token == "" {
		return cursorState{}, cursorState{}
	}
	n, err := strconv.ParseUint(token, 10, 64)
	if err != nil {
		return cursorState{}, cursorState{}
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	st, ok := ls.active[n]
	if !ok {
		return cursorState{}, cursorState{}
	}
	st.expires = time.Now().Add(listSessionTTL)
	ls.active[n] = st
	return st.overlay, st.baseline
}

func (ls *listSessions) gcLocked() {
	now := time.Now()
	for id, st := range ls.active {
		if now.After(st.expires) {
			delete(ls.active, id)
		}
	}
}

// overlayStore presents a write-coalescing overlay: every write lands on the
// overlay store, reads prefer the overlay store and fall back to the
// baseline, and listings merge the two (overlay keys win on ties).
type overlayStore struct {
	overlay  Store // all writes land here
	baseline Store // reads fall back here; never modified
	sessions *listSessions
}

func newOverlayStore(overlay, baseline Store) *overlayStore {
	return &overlayStore{overlay: overlay, baseline: baseline, sessions: newListSessions()}
}

func (o *overlayStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, *ObjectMeta, error) {
	rc, meta, err := o.overlay.Get(ctx, bucket, key)
	if err == nil {
		return rc, meta, nil
	}
	if err != ErrNotFound {
		return nil, nil, err
	}
	return o.baseline.Get(ctx, bucket, key)
}

func (o *overlayStore) Head(ctx context.Context, bucket, key string) (*ObjectMeta, error) {
	meta, err := o.overlay.Head(ctx, bucket, key)
	if err == nil {
		return meta, nil
	}
	if err != ErrNotFound {
		return nil, err
	}
	return o.baseline.Head(ctx, bucket, key)
}

func (o *overlayStore) Put(ctx context.Context, bucket, key string, body io.Reader, contentType string) (*ObjectMeta, error) {
	return o.overlay.Put(ctx, bucket, key, body, contentType)
}

func (o *overlayStore) List(ctx context.Context, bucket string) ([]ObjectMeta, error) {
	var out []ObjectMeta
	after := ""
	for {
		page, err := o.ListPage(ctx, bucket, ListParams{After: after, MaxKeys: 1000})
		if err != nil {
			return nil, err
		}
		out = append(out, page.Objects...)
		if !page.Truncated {
			break
		}
		if page.NextToken == "" {
			return nil, fmt.Errorf("list %s: truncated response without continuation token", bucket)
		}
		after = page.NextToken
	}
	return out, nil
}

// mergePageEntries merges the (individually sorted) objects and common
// prefixes of one backend page into a single key-ordered entry list.
func mergePageEntries(page ListPage) []listEntry {
	entries := make([]listEntry, 0, len(page.Objects)+len(page.CommonPrefixes))
	oi, pi := 0, 0
	for oi < len(page.Objects) || pi < len(page.CommonPrefixes) {
		if pi >= len(page.CommonPrefixes) ||
			(oi < len(page.Objects) && page.Objects[oi].Key <= page.CommonPrefixes[pi]) {
			obj := page.Objects[oi]
			entries = append(entries, listEntry{key: obj.Key, obj: &obj})
			oi++
		} else {
			entries = append(entries, listEntry{key: page.CommonPrefixes[pi], prefix: true})
			pi++
		}
	}
	return entries
}

// listCursor streams one backend's listing. It resumes from a cursorState
// (unconsumed tail + backend token), fetches further pages only once the
// tail is consumed, and snapshots its position afterwards.
// listCursor streams one backend's listing. A backend that does not hold
// the bucket at all streams as empty (the client bucket may live only on the
// other side); the overlay merge reports NoSuchBucket only when both sides
// are missing.
type listCursor struct {
	store   Store
	bucket  string
	p       ListParams
	after   string
	entries []listEntry
	idx     int
	done    bool
	missing bool
}

func newListCursor(store Store, bucket string, p ListParams, st cursorState) *listCursor {
	return &listCursor{
		store: store, bucket: bucket, p: p,
		after: st.after, entries: st.remaining, done: st.done, missing: st.missing,
	}
}

// load fetches the next backend page, but only once the current entries
// (restored from a previous session or a freshly fetched page) are consumed.
func (c *listCursor) load(ctx context.Context) error {
	for c.idx >= len(c.entries) && !c.done {
		page, err := c.store.ListPage(ctx, c.bucket, ListParams{
			Prefix:    c.p.Prefix,
			Delimiter: c.p.Delimiter,
			After:     c.after,
			MaxKeys:   c.p.MaxKeys,
		})
		if err != nil {
			// a backend that does not hold the bucket at all streams as empty
			if errors.Is(err, ErrNotFound) {
				c.done = true
				c.missing = true
				return nil
			}
			return err
		}
		c.entries = mergePageEntries(page)
		c.idx = 0
		c.after = page.NextToken
		c.done = !page.Truncated
	}
	return nil
}

func (c *listCursor) peek(ctx context.Context) (listEntry, bool, error) {
	if err := c.load(ctx); err != nil {
		return listEntry{}, false, err
	}
	if c.idx >= len(c.entries) {
		return listEntry{}, false, nil
	}
	return c.entries[c.idx], true, nil
}

func (c *listCursor) next() { c.idx++ }

func (c *listCursor) snapshot() cursorState {
	rest := make([]listEntry, 0, len(c.entries)-c.idx)
	rest = append(rest, c.entries[c.idx:]...)
	return cursorState{after: c.after, remaining: rest, done: c.done, missing: c.missing}
}

// ListPage merges the overlay and baseline listings on the fly: both are
// key-ordered, so the k-way merge streams entries without materializing the
// full bucket. The returned token is a session ID mapping to both backends'
// resume positions.
func (o *overlayStore) ListPage(ctx context.Context, bucket string, p ListParams) (ListPage, error) {
	if p.MaxKeys <= 0 {
		p.MaxKeys = 1000
	}
	ovState, bsState := o.sessions.lookup(p.After)
	oc := newListCursor(o.overlay, bucket, p, ovState)
	bc := newListCursor(o.baseline, bucket, p, bsState)

	var out ListPage
	appendEntry := func(e listEntry) {
		if e.prefix {
			out.CommonPrefixes = append(out.CommonPrefixes, e.key)
		} else {
			out.Objects = append(out.Objects, *e.obj)
		}
	}
	for len(out.Objects)+len(out.CommonPrefixes) < p.MaxKeys {
		oe, oHas, err := oc.peek(ctx)
		if err != nil {
			return ListPage{}, err
		}
		be, bHas, err := bc.peek(ctx)
		if err != nil {
			return ListPage{}, err
		}
		switch {
		case oHas && bHas:
			if oe.key <= be.key {
				appendEntry(oe)
				oc.next()
				if oe.key == be.key {
					bc.next()
				}
			} else {
				appendEntry(be)
				bc.next()
			}
		case oHas:
			appendEntry(oe)
			oc.next()
		case bHas:
			appendEntry(be)
			bc.next()
		default:
			return o.finishListPage(ctx, out, oc, bc)
		}
	}

	more := false
	for _, c := range []*listCursor{oc, bc} {
		if _, has, err := c.peek(ctx); err != nil {
			return ListPage{}, err
		} else if has {
			more = true
		}
	}
	if more {
		out.Truncated = true
		out.NextToken = o.sessions.create(oc.snapshot(), bc.snapshot())
		return out, nil
	}
	if len(out.Objects) == 0 && len(out.CommonPrefixes) == 0 && oc.missing && bc.missing {
		return ListPage{}, ErrNotFound
	}
	return out, nil
}

// finishListPage handles the empty-page case for the exhausted-both branch.
func (o *overlayStore) finishListPage(ctx context.Context, out ListPage, oc, bc *listCursor) (ListPage, error) {
	if len(out.Objects) == 0 && len(out.CommonPrefixes) == 0 && oc.missing && bc.missing {
		return ListPage{}, ErrNotFound
	}
	return out, nil
}

func (o *overlayStore) ListBuckets(ctx context.Context) ([]string, error) {
	overlay, err := o.overlay.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}
	baseline, err := o.baseline.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(overlay)+len(baseline))
	for _, b := range overlay {
		set[b] = true
	}
	for _, b := range baseline {
		set[b] = true
	}
	out := make([]string, 0, len(set))
	for b := range set {
		out = append(out, b)
	}
	sort.Strings(out)
	return out, nil
}

func (o *overlayStore) BucketExists(ctx context.Context, bucket string) (bool, error) {
	ok, err := o.overlay.BucketExists(ctx, bucket)
	if err != nil || ok {
		return ok, err
	}
	// the baseline may serve the bucket read-only; probe it, but do not
	// leak its permission or existence errors — an unverifiable baseline
	// simply means the bucket is not present as far as the gateway knows
	ok, err = o.baseline.BucketExists(ctx, bucket)
	if err != nil {
		return false, nil
	}
	return ok, nil
}

func (o *overlayStore) CreateBucket(ctx context.Context, bucket string) error {
	return o.overlay.CreateBucket(ctx, bucket)
}

func (o *overlayStore) InitiateMultipart(ctx context.Context, bucket, key, contentType string) (string, error) {
	return o.overlay.InitiateMultipart(ctx, bucket, key, contentType)
}

func (o *overlayStore) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int32, body io.Reader) (string, error) {
	return o.overlay.UploadPart(ctx, bucket, key, uploadID, partNumber, body)
}

func (o *overlayStore) CompleteMultipart(ctx context.Context, bucket, key, uploadID string, parts []CompletedPart) (*ObjectMeta, error) {
	return o.overlay.CompleteMultipart(ctx, bucket, key, uploadID, parts)
}

func (o *overlayStore) AbortMultipart(ctx context.Context, bucket, key, uploadID string) error {
	return o.overlay.AbortMultipart(ctx, bucket, key, uploadID)
}

func (o *overlayStore) ListParts(ctx context.Context, bucket, key, uploadID string) ([]PartInfo, error) {
	return o.overlay.ListParts(ctx, bucket, key, uploadID)
}

func (o *overlayStore) ListMultipartUploads(ctx context.Context, bucket string) ([]MultipartInfo, error) {
	return o.overlay.ListMultipartUploads(ctx, bucket)
}
