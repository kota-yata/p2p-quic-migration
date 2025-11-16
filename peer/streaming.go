package main

import (
    "context"
    "log"
    "sync"
    "time"

    "github.com/quic-go/quic-go"
)

// Initiator: Opens streams first, then accepts return streams
func handleBidirectionalCommunicationAsInitiator(conn *quic.Conn, peerAddr string) {
    defer conn.CloseWithError(0, "Initiator session completed")
    log.Printf("Starting bidirectional communication as initiator with %s", peerAddr)

    // First: Open our outgoing streams
    audioSendStream, err := conn.OpenStreamSync(context.Background())
    if err != nil {
        log.Printf("Failed to open outgoing audio stream as initiator: %v", err)
        return
    }
    defer audioSendStream.Close()

    videoSendStream, err := conn.OpenStreamSync(context.Background())
    if err != nil {
        log.Printf("Failed to open outgoing video stream as initiator: %v", err)
        return
    }
    defer videoSendStream.Close()

    log.Printf("Initiator opened outgoing streams, starting to send audio/video")

    var wg sync.WaitGroup

    // Start sending our audio and video
    wg.Add(2)
    go func() {
        defer wg.Done()
        audioStreamer := NewAudioStreamer(audioSendStream)
        if err := audioStreamer.StreamAudio(); err != nil {
            log.Printf("Initiator audio streaming failed: %v", err)
        }
    }()

    go func() {
        defer wg.Done()
        videoStreamer := NewVideoStreamer(videoSendStream)
        if err := videoStreamer.StreamVideo(); err != nil {
            log.Printf("Initiator video streaming failed: %v", err)
        }
    }()

    // Then: Accept return streams from the acceptor
    go func() {
        log.Printf("Initiator waiting for return streams from acceptor...")
        acceptStreamsFromPeer(conn, "initiator")
    }()

    wg.Wait()
    log.Printf("Initiator bidirectional communication completed")
}

// Acceptor: Accepts streams first, then opens return streams
func handleBidirectionalCommunicationAsAcceptor(conn *quic.Conn) {
    log.Printf("Starting bidirectional communication as acceptor")

    // First: Accept incoming streams from initiator
    go func() {
        log.Printf("Acceptor waiting for incoming streams from initiator...")
        acceptStreamsFromPeer(conn, "acceptor")
    }()

    // Give the acceptor goroutine a moment to start, then open our return streams
    time.Sleep(100 * time.Millisecond)

    audioSendStream, err := conn.OpenStreamSync(context.Background())
    if err != nil {
        log.Printf("Failed to open return audio stream as acceptor: %v", err)
        return
    }
    defer audioSendStream.Close()

    videoSendStream, err := conn.OpenStreamSync(context.Background())
    if err != nil {
        log.Printf("Failed to open return video stream as acceptor: %v", err)
        return
    }
    defer videoSendStream.Close()

    log.Printf("Acceptor opened return streams, starting to send audio/video back")

    var wg sync.WaitGroup
    wg.Add(2)

    go func() {
        defer wg.Done()
        audioStreamer := NewAudioStreamer(audioSendStream)
        if err := audioStreamer.StreamAudio(); err != nil {
            log.Printf("Acceptor audio streaming failed: %v", err)
        }
    }()

    go func() {
        defer wg.Done()
        videoStreamer := NewVideoStreamer(videoSendStream)
        if err := videoStreamer.StreamVideo(); err != nil {
            log.Printf("Acceptor video streaming failed: %v", err)
        }
    }()

    wg.Wait()
    log.Printf("Acceptor bidirectional communication completed")
}

// Common function to accept and handle incoming streams
func acceptStreamsFromPeer(conn *quic.Conn, role string) {
    streamCount := 0
    for {
        stream, err := conn.AcceptStream(context.Background())
        if err != nil {
            log.Printf("%s error accepting incoming stream: %v", role, err)
            break
        }

        streamCount++
        log.Printf("%s accepted incoming stream #%d", role, streamCount)

        if streamCount == 1 {
            go handleIncomingAudioStream(stream, role)
        } else if streamCount == 2 {
            go handleIncomingVideoStream(stream, role)
        } else {
            log.Printf("%s unexpected additional stream #%d, closing", role, streamCount)
            stream.Close()
        }
    }
}

func handleIncomingAudioStream(stream *quic.Stream, role string) {
    defer stream.Close()
    log.Printf("%s starting to receive and play incoming audio stream", role)

    audioReceiver := NewAudioReceiver(stream)
    if err := audioReceiver.ReceiveAudio(); err != nil {
        log.Printf("%s audio receiving failed: %v", role, err)
    } else {
        log.Printf("%s audio receiving completed successfully", role)
    }
}

func handleIncomingVideoStream(stream *quic.Stream, role string) {
    defer stream.Close()
    log.Printf("%s starting to receive and play incoming video stream", role)

    videoReceiver := NewVideoReceiver(stream)
    if err := videoReceiver.ReceiveVideo(); err != nil {
        log.Printf("%s video receiving failed: %v", role, err)
    } else {
        log.Printf("%s video receiving completed successfully", role)
    }
}

