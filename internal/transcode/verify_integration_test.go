//go:build integration

package transcode

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/vinhpnguyen123/goalstream/internal/storage"
)

// Verify on a catalogue with one clip broken each way: only the broken clips are reported, each
// with what is wrong, and no line carries a presigned signature.
func TestVerify(t *testing.T) {
	ctx := context.Background()
	e := setupJobs(t)
	raw := storage.RawClient(e.cfg.S3, e.cfg.S3.Endpoint)
	del := func(key string) {
		t.Helper()
		if _, err := raw.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(e.cfg.S3.Bucket), Key: aws.String(key)}); err != nil {
			t.Fatal(err)
		}
	}
	// live transcodes a clip for real and returns its live version prefix and rungs, tallest first.
	live := func() (id, base string, rungs []verifyRung) {
		t.Helper()
		id, prefix := e.clip(t, "clip_576p.mp4")
		if err := e.w.Process(ctx, e.job(t, id)); err != nil {
			t.Fatal(err)
		}
		_, ver := e.delivery(t, id)
		rows, err := e.pool.Query(ctx, `SELECT height, segment_count FROM renditions WHERE clip_id = $1 ORDER BY height DESC`, id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var r verifyRung
			if err := rows.Scan(&r.height, &r.segments); err != nil {
				t.Fatal(err)
			}
			rungs = append(rungs, r)
		}
		if len(rungs) < 2 {
			t.Fatalf("fixture produced %d rungs, the test needs 2+", len(rungs))
		}
		return id, prefix + "hls/" + ver + "/", rungs
	}

	good, _, _ := live()
	lostSeg, base, segRungs := live()
	top := fmt.Sprintf("%dp", segRungs[0].height)
	del(base + top + "/seg_00001.ts")
	lostVariant, base, rungs := live()
	bottom := fmt.Sprintf("%dp", rungs[len(rungs)-1].height)
	del(base + bottom + "/index.m3u8")
	noMaster, base, _ := live()
	del(base + "master.m3u8")
	garbage, base, _ := live() // ffprobe fails, and its error echoes the URL it was given
	if _, err := raw.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(e.cfg.S3.Bucket), Key: aws.String(base + "master.m3u8"),
		Body: strings.NewReader("not a playlist")}); err != nil {
		t.Fatal(err)
	}
	// The review's case: a later segment renamed, so the folder still holds as many objects —
	// a folder count passes it; the playlist still names the old key, which now 404s.
	renamed, base, renRungs := live()
	rtop := fmt.Sprintf("%dp", renRungs[0].height)
	last := fmt.Sprintf("seg_%05d.ts", renRungs[0].segments-1)
	if _, err := raw.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(e.cfg.S3.Bucket),
		CopySource: aws.String(e.cfg.S3.Bucket + "/" + base + rtop + "/" + last), Key: aws.String(base + rtop + "/seg_99999.ts")}); err != nil {
		t.Fatal(err)
	}
	del(base + rtop + "/" + last)
	miscounted, _, misRungs := live() // the commit recorded one segment more than the playlist lists
	mtop := fmt.Sprintf("%dp", misRungs[0].height)
	if _, err := e.pool.Exec(ctx, `UPDATE renditions SET segment_count = segment_count + 1 WHERE clip_id = $1 AND height = $2`,
		miscounted, misRungs[0].height); err != nil {
		t.Fatal(err)
	}
	plainMP4, _ := e.clip(t, "clip_576p.mp4")
	noOriginal, _ := e.clip(t, "")

	hlsBase := os.Getenv("HLS_BASE_URL")
	if hlsBase == "" {
		t.Fatal("HLS_BASE_URL must point at the local nginx (make up)")
	}
	r, err := Verify(ctx, e.pool, e.w.Store, hlsBase+"/"+e.cfg.S3.Bucket)
	if err != nil {
		t.Fatal(err)
	}
	if r.HLS != 7 || r.MP4 != 2 {
		t.Fatalf("checked mp4 %d / hls %d, want 2 / 7", r.MP4, r.HLS)
	}
	got := map[string][]string{}
	for _, p := range r.Problems {
		got[p.ClipID] = append(got[p.ClipID], p.What)
		if strings.Contains(p.What, "X-Amz") || strings.Contains(p.What, "Signature") {
			t.Errorf("problem line leaks a presigned URL: %s", p.What)
		}
	}
	for _, id := range []string{good, plainMP4} {
		if len(got[id]) != 0 {
			t.Errorf("healthy clip %s reported: %v", id, got[id])
		}
	}
	want := map[string][]string{
		lostSeg:     {fmt.Sprintf("%s/index.m3u8: 1 of %d referenced segments missing (first: seg_00001.ts)", top, segRungs[0].segments)},
		renamed:     {fmt.Sprintf("%s/index.m3u8: 1 of %d referenced segments missing (first: %s)", rtop, renRungs[0].segments, last)},
		miscounted:  {fmt.Sprintf("%s/index.m3u8 lists %d segments, renditions says %d", mtop, misRungs[0].segments, misRungs[0].segments+1)},
		lostVariant: {bottom + "/index.m3u8 missing", "ffprobe"},
		noMaster:    {"master.m3u8 missing"},
		garbage:     {"ffprobe master: "},
		noOriginal:  {"original.mp4 missing"},
	}
	for id, subs := range want {
		if len(got[id]) != len(subs) {
			t.Errorf("clip %s: problems %q, want %d matching %q", id, got[id], len(subs), subs)
			continue
		}
		for i, sub := range subs {
			if !strings.Contains(got[id][i], sub) {
				t.Errorf("clip %s problem %d = %q, want it to contain %q", id, i, got[id][i], sub)
			}
		}
	}
	t.Logf("lost variant: %q", got[lostVariant])
	t.Logf("garbage master: %q", got[garbage])
}
