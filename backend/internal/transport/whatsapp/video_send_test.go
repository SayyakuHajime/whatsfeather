package whatsapp

import (
	"testing"
)

func TestBuildOutgoingVideoMessagePreservesUploadDescriptorAndCaption(t *testing.T) {
	url := "https://mmg.whatsapp.net/synthetic/video"
	directPath := "/v/t62.7161-24/synthetic"
	mediaKey := []byte("synthetic-media-key")
	fileEncSHA256 := []byte("synthetic-encrypted-hash")
	fileSHA256 := []byte("synthetic-plain-hash")
	const length uint64 = 123456
	message := buildVideoMessage(url, directPath, mediaKey, fileEncSHA256, fileSHA256, length, "video/mp4", "caption from composer")
	if message == nil || message.VideoMessage == nil || message.ImageMessage != nil || message.DocumentMessage != nil {
		t.Fatalf("outgoing payload is not a VideoMessage: %#v", message)
	}
	video := message.GetVideoMessage()
	if video.GetURL() != url || video.GetDirectPath() != directPath || string(video.GetMediaKey()) != string(mediaKey) ||
		string(video.GetFileEncSHA256()) != string(fileEncSHA256) || string(video.GetFileSHA256()) != string(fileSHA256) ||
		video.GetFileLength() != length || video.GetMimetype() != "video/mp4" || video.GetCaption() != "caption from composer" {
		t.Fatalf("VideoMessage lost upload response fields or caption: %#v", video)
	}
	if video.GifPlayback != nil || video.GetGifPlayback() {
		t.Fatalf("ordinary video must not set GIF playback: %#v", video.GifPlayback)
	}
}
