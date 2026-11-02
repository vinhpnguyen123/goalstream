package transcode

import "testing"

func TestParseProbe(t *testing.T) {
	cases := []struct {
		name string
		json string
		want Probe
	}{
		{
			// Shape of a real SoccerNet half (DATASET.md): MKV, no per-stream bit_rate,
			// Vorbis audio, an ASS subtitle stream.
			"soccernet mkv half",
			`{"streams":[{"codec_type":"video","width":1280,"height":720},
			             {"codec_type":"audio"},{"codec_type":"subtitle"}],
			  "format":{"duration":"2725.004000","bit_rate":"2824198"}}`,
			Probe{HasVideo: true, HasAudio: true, Width: 1280, Height: 720, FormatBps: 2824198, DurationMS: 2725004},
		},
		{
			"mezzanine mp4",
			`{"streams":[{"codec_type":"video","width":1280,"height":720,"bit_rate":"4509448"},
			             {"codec_type":"audio","bit_rate":"128498"}],
			  "format":{"duration":"60.000000","bit_rate":"4644149"}}`,
			Probe{HasVideo: true, HasAudio: true, Width: 1280, Height: 720, VideoBps: 4509448, AudioBps: 128498, FormatBps: 4644149, DurationMS: 60000},
		},
		{
			"no audio",
			`{"streams":[{"codec_type":"video","width":320,"height":180,"bit_rate":"N/A"}],"format":{"duration":"3.0"}}`,
			Probe{HasVideo: true, Width: 320, Height: 180, DurationMS: 3000},
		},
		{
			"second video stream ignored",
			`{"streams":[{"codec_type":"video","width":640,"height":360},{"codec_type":"video","width":1920,"height":1080}],"format":{}}`,
			Probe{HasVideo: true, Width: 640, Height: 360},
		},
		{"empty", `{"streams":[],"format":{}}`, Probe{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProbe([]byte(tc.json))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %+v\nwant %+v", got, tc.want)
			}
		})
	}
	if _, err := parseProbe([]byte("not json")); err == nil {
		t.Fatal("garbage must be an error")
	}
}

func TestTail(t *testing.T) {
	big := make([]byte, 20<<10)
	big[len(big)-1] = 'x'
	got := Tail(big)
	if len(got) != 8<<10 || got[len(got)-1] != 'x' {
		t.Fatalf("Tail kept %d bytes", len(got))
	}
	if string(Tail([]byte("short"))) != "short" {
		t.Fatal("short input must be unchanged")
	}
}
