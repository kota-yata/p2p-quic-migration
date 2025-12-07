package main

import (
    "context"
    "log"
    "sync"
    "time"

    "github.com/quic-go/quic-go"
)

// Initiator: behavior depends on role
func handleCommunicationAsInitiator(conn *quic.Conn, peerAddr string, role string) {
    defer conn.CloseWithError(0, "Initiator session completed")
    log.Printf("Initiator started with role=%s, peer=%s", role, peerAddr)

    var wg sync.WaitGroup

    // If sender or both: open outgoing stream and send audio
    if role == "sender" || role == "both" {
        audioSendStream, err := conn.OpenStreamSync(context.Background())
        if err != nil {
            log.Printf("Failed to open outgoing audio stream as initiator: %v", err)
        } else {
            log.Printf("Initiator opened outgoing audio stream, starting to send audio")
            wg.Add(1)
            go func() {
                defer wg.Done()
                defer audioSendStream.Close()
                audioStreamer := NewAudioStreamer(audioSendStream)
                if err := audioStreamer.StreamAudio(); err != nil {
                    log.Printf("Initiator audio streaming failed: %v", err)
                }
            }()
        }
    }

    // If receiver or both: accept incoming streams
    if role == "receiver" || role == "both" {
        go func() {
            log.Printf("Initiator waiting to receive incoming audio stream from peer...")
            acceptStreamsFromPeer(conn, "initiator", role)
        }()
    }

    wg.Wait()
    log.Printf("Initiator communication completed for role=%s", role)
}

// Acceptor: behavior depends on role
func handleCommunicationAsAcceptor(conn *quic.Conn, role string) {
    log.Printf("Acceptor started with role=%s", role)

    // If receiver or both: accept incoming streams first
    if role == "receiver" || role == "both" {
        go func() {
            log.Printf("Acceptor waiting for incoming audio stream from initiator...")
            acceptStreamsFromPeer(conn, "acceptor", role)
        }()
    }

    // If sender or both: open outgoing stream to send
    if role == "sender" || role == "both" {
        // Give the accept goroutine a moment if also receiving
        if role == "both" {
            time.Sleep(100 * time.Millisecond)
        }

        audioSendStream, err := conn.OpenStreamSync(context.Background())
        if err != nil {
            log.Printf("Failed to open outgoing audio stream as acceptor: %v", err)
            return
        }
        defer audioSendStream.Close()

        var wg sync.WaitGroup
        wg.Add(1)

        go func() {
            defer wg.Done()
            audioStreamer := NewAudioStreamer(audioSendStream)
            if err := audioStreamer.StreamAudio(); err != nil {
                log.Printf("Acceptor audio streaming failed: %v", err)
            }
        }()

        wg.Wait()
        log.Printf("Acceptor sending completed for role=%s", role)
    }
}

// Common function to accept and handle incoming streams
func acceptStreamsFromPeer(conn *quic.Conn, who string, role string) {
    streamCount := 0
    for {
        stream, err := conn.AcceptStream(context.Background())
        if err != nil {
            log.Printf("%s error accepting incoming stream: %v", who, err)
            break
        }

        streamCount++
        log.Printf("%s accepted incoming stream #%d", who, streamCount)

        if streamCount == 1 {
            // Only receive if role permits
            if role == "receiver" || role == "both" {
                go handleIncomingAudioStream(stream, who)
            } else {
                log.Printf("%s is sender-only; closing unexpected inbound stream #%d", who, streamCount)
                stream.Close()
            }
        } else {
            log.Printf("%s unexpected additional stream #%d (video disabled), closing", who, streamCount)
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
