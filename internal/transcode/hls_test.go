package transcode

import "testing"

func TestMasterPlaylist(t *testing.T) {
	rungs, err := Ladder(1_372_000, 720) // §10 example A
	if err != nil {
		t.Fatal(err)
	}
	widths := []int{1280, 854, 640}
	var rds []Rendition
	for i, r := range rungs {
		rds = append(rds, Rendition{Rung: r, Width: widths[i], Segments: 15})
	}
	want := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-STREAM-INF:BANDWIDTH=1675842,AVERAGE-BANDWIDTH=1500000,RESOLUTION=1280x720,CODECS="avc1.640028,mp4a.40.2"
720p/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=871521,AVERAGE-BANDWIDTH=782000,RESOLUTION=854x480,CODECS="avc1.640028,mp4a.40.2"
480p/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=486160,AVERAGE-BANDWIDTH=439000,RESOLUTION=640x360,CODECS="avc1.640028,mp4a.40.2"
360p/index.m3u8
`
	if got := string(MasterPlaylist(rds)); got != want {
		t.Fatalf("master:\n%s\nwant:\n%s", got, want)
	}
}
