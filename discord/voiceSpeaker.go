package discord

import (
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/denverquane/amongusdiscord/locale"
	"github.com/denverquane/amongusdiscord/storage"
	"github.com/jonas747/dca"
)

// VoicePhrase type
type VoicePhrase int

// VoicePhrase constants
const (
	WelcomeToLobby VoicePhrase = iota
	NewRoomCode
	CaptureConnected
	SeeYouLater
)

type VoicePhraseString string

var VoicePhraseNames = map[VoicePhrase]VoicePhraseString{
	WelcomeToLobby:   "WelcomeToLobby",
	NewRoomCode:      "NewRoomCode",
	CaptureConnected: "CaptureConnected",
	SeeYouLater:      "SeeYouLater",
}

func (phrase *VoicePhrase) ToString() VoicePhraseString {
	return VoicePhraseNames[*phrase]
}

//
type VoiceManager struct {
	Sessions map[string]VoiceService
}

type VoiceService interface {
	GetChannelID() string
	Connect(s *discordgo.Session, g *discordgo.Guild, m *discordgo.MessageCreate) (err error)
	Speak(phrase VoicePhrase) (msg, err error)
}

func NewVoiceManager() *VoiceManager {
	return &VoiceManager{
		Sessions: make(map[string]VoiceService),
	}
}

func (vm VoiceManager) AddSession(s *discordgo.Session, g *discordgo.Guild, ChannelID string, sett *storage.GuildSettings) VoiceService {
	if vm.Sessions[g.ID] != nil {
		return vm.Sessions[g.ID]
	}

	voice := newVoice(ChannelID, sett)
	vm.Sessions[g.ID] = &voice

	return &voice
}

func (vm VoiceManager) DeleteSession(guildID string) {
	delete(vm.Sessions, guildID)
}

// TODO: Remake this!
func (vm VoiceManager) GetByChannelID(channelID string) VoiceService {
	for _, voice := range vm.Sessions {
		if voice.GetChannelID() == channelID {
			return voice
		}
	}

	return nil
}

//

type Voice struct {
	sync.Mutex

	VoiceConnection  *discordgo.VoiceConnection `json:"voiceConnection"`
	EncodingSession  *dca.EncodeSession         `json:"encodingSession"`
	StreamingSession *dca.StreamingSession      `json:"streamingSession"`
	EncodingOptions  *dca.EncodeOptions         `json:"encodingOptions"`

	Queue     []*VoicePhrase
	done      chan error
	sett      *storage.GuildSettings
	ChannelID string
}

func newVoice(channelID string, sett *storage.GuildSettings) Voice {
	return Voice{
		VoiceConnection:  nil,
		EncodingSession:  nil,
		StreamingSession: nil,
		EncodingOptions:  GetDefaultAudioEncoding(),
		sett:             sett,
		done:             make(chan error),
		ChannelID:        channelID,
	}
}

func (v *Voice) GetChannelID() string {
	return v.ChannelID
}

func (voice *Voice) Connect(s *discordgo.Session, g *discordgo.Guild, m *discordgo.MessageCreate) (err error) {
	if !voice.sett.GetBotSpeech() {
		return
	}

	voice.Lock()
	defer voice.Unlock()

	channel := voice.FindChannel(s, m, g)

	if channel.ChannelName == "" {
		return fmt.Errorf("Chanel not found")
	}

	if !voice.IsConnected() {
		voice.VoiceConnection, err = s.ChannelVoiceJoin(g.ID, channel.ChannelID, false, true)
		if err != nil {
			return
		}
	} else if channel.ChannelID != voice.VoiceConnection.ChannelID {
		err = voice.VoiceConnection.ChangeChannel(channel.ChannelID, false, true)
		if err != nil {
			return
		}
	}

	if qPhrase := voice.QueueGetNext(); qPhrase != nil && !voice.IsStreaming() {
		voice.Speak(*qPhrase)
	}

	return nil
}

func (voice *Voice) FindChannel(s *discordgo.Session, m *discordgo.MessageCreate, g *discordgo.Guild) (tracking TrackingChannel) {
	tracking = TrackingChannel{}

	channels, err := s.GuildChannels(m.GuildID)
	if err != nil {
		log.Println(err)
	}

	// loop over all the channels in the discord and cross-reference with the one that the .au new author is in
	for _, channel := range channels {
		if channel.Type == discordgo.ChannelTypeGuildVoice {
			for _, v := range g.VoiceStates {
				// if the User who typed au new is in a voice channel
				if v.UserID == m.Author.ID {
					// once we find the voice channel
					if channel.ID == v.ChannelID {
						tracking = TrackingChannel{
							ChannelID:   channel.ID,
							ChannelName: channel.Name,
						}
						return
					}
				}
			}
		}
	}

	return
}

// Disconnect from the current voice channel
func (voice *Voice) Disconnect() (err error) {
	voice.Lock()
	defer voice.Unlock()

	if !voice.IsConnected() {
		return
	}

	err = voice.stop()
	if err != nil {
		return
	}

	// Leave the voice channel
	err = voice.VoiceConnection.Disconnect()
	if err != nil {
		return
	}

	// Clear the old voice connection in memory
	voice.VoiceConnection = nil

	// Leaving the voice channel worked out fine
	return nil
}

// Stop stops the playback of a media
func (voice *Voice) stop() error {
	// Make sure we're streaming first
	if !voice.IsStreaming() {
		return nil
	}

	voice.done <- nil

	// Stop the encoding session
	if err := voice.EncodingSession.Stop(); err != nil {
		return err
	}

	// Clean up the encoding session
	voice.EncodingSession.Cleanup()

	return nil
}

// Stop stops the playback of a media
func (voice *Voice) Stop() error {
	voice.Lock()
	defer voice.Unlock()

	return voice.stop()
}

func fileExists(name string) bool {
	_, err := os.Stat(name)
	return !os.IsNotExist(err)
}

// Play a given phrase in a connected voice channel
func (voice *Voice) Speak(phrase VoicePhrase) (msg, err error) {
	log.Println("Try Speak: " + string(phrase.ToString()))

	// Make sure we're connected first
	if !voice.IsConnected() {
		// log.Println("Not connected")
		return
	}

	if voice.IsStreaming() {
		voice.QueueAdd(&phrase)
		// log.Println("in stream..")
		return
	}

	voice.Lock()

	// TODO: check for file existence

	fileName := fmt.Sprintf("sounds/%s.%s.mp3", string(phrase.ToString()), voice.sett.GetLanguage())
	if !fileExists(fileName) {
		// try get default lang
		fileName := fmt.Sprintf("sounds/%s.%s.mp3", string(phrase.ToString()), locale.DefaultLang)
		if !fileExists(fileName) {
			return
		}
	}

	voice.EncodingSession, err = dca.EncodeFile(fileName, voice.EncodingOptions)

	if err != nil {
		return
	}

	log.Println("The start: " + fileName)

	voice.Speaking()
	defer voice.Silent()

	voice.done = make(chan error)
	voice.StreamingSession = dca.NewStream(voice.EncodingSession, voice.VoiceConnection, voice.done)
	voice.Unlock()

	// Wait for the streaming session to finish
	// msg = <-voice.done
	// log.Println("The end. #1")

	// TODO: make a timer if the film is clamped
	// ticker := time.NewTicker(time.Second)
	select {
	case msg = <-voice.done:
	case <-time.After(8 * time.Second):
		log.Println("out of time :(")
		break
	}

	voice.Lock()

	// Figure out why the streaming session stopped
	_, err = voice.StreamingSession.Finished()

	// Clean up the streaming session
	voice.StreamingSession = nil

	// Clean up the encoding session
	voice.EncodingSession.Stop()
	voice.EncodingSession.Cleanup()
	voice.EncodingSession = nil

	voice.Unlock()

	if qPhrase := voice.QueueGetNext(); qPhrase != nil {
		return voice.Speak(*qPhrase)
	} else if phrase == SeeYouLater {
		voice.Disconnect()
	}
	// log.Println("The end. #1")

	return
}

func (voice *Voice) QueueAdd(phrase *VoicePhrase) {
	voice.Queue = append(voice.Queue, phrase)
}

func (voice *Voice) QueueRemove(phrase int) {
	voice.Queue = append(voice.Queue[:phrase], voice.Queue[phrase+1:]...)
}

func (voice *Voice) QueueGetNext() (phrase *VoicePhrase) {
	if len(voice.Queue) == 0 {
		return nil
	}
	phrase = voice.Queue[0]
	voice.QueueRemove(0)
	return
}

// Speaking allows the sending of audio to Discord
func (voice *Voice) Speaking() {
	if voice.IsConnected() {
		voice.VoiceConnection.Speaking(true)
	}
}

// Silent prevents the sending of audio to Discord
func (voice *Voice) Silent() {
	if voice.IsConnected() {
		voice.VoiceConnection.Speaking(false)
	}
}

// IsConnected returns whether or not a voice connection exists
func (voice *Voice) IsConnected() bool {
	return voice.VoiceConnection != nil
}

func GetDefaultAudioEncoding() *dca.EncodeOptions {
	opts := dca.StdEncodeOptions
	opts.RawOutput = true
	opts.Bitrate = 128
	opts.Volume = 220
	return opts
}

func (voice *Voice) IsStreaming() bool {
	if !voice.IsConnected() {
		return false
	}

	if voice.StreamingSession == nil {
		return false
	}

	if voice.EncodingSession == nil {
		return false
	}

	return true
}
