package main

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"os/exec"
	"strings"
	"sync"

	"github.com/LordOfTrident/ansi-go"
	gostream "github.com/libp2p/go-libp2p-gostream"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"golang.design/x/clipboard"

	"github.com/diniamo/gopv"
)

type server struct {
	// The mpv process.
	mpv *exec.Cmd
	// The IPC client.
	ipc *gopv.Client
	// The debouncer used for ignoring events that were caused by our own requests.
	debouncer debouncer

	// The clients that are currently connected.
	// A map is used to allow for easy removal.
	clients map[int]*serverClient
	// The ID the next client is going be assigned.
	// This is used to key clients.
	clientID int

	// The channel used to funnel server actions into the main thread.
	// This greatly simplifies concurrency by grouping all mutation into the same thread.
	// See the cmd* types below.
	cmdChan chan any
	// Whether the server is ready, which is required for the wait state to end
	ready bool
	// Whether the session is currently waiting.
	// All media controls should be blocked, except speed.
	waiting bool
	// The time at which the wait started.
	// Used for preventing unpauses by seeking back to here.
	waitTime float64
	// The number of clients still waiting.
	waitCount int
	// Whether playback should be resumed after the wait.
	waitResume bool

	// The fileserver.
	fileServer *fileServer

	// The title of the playing media.
	// Should be empty when the media is remote.
	title string
	// The URL of the playing media.
	// Should be empty when the media is local.
	url string
	// Whether playback is paused.
	// If we send a request to pause or resume, this should be updated next to that request,
	// because we may rely on this state before the pause event gets processed, and in this case
	// the next appropriate event has to be debounced as well.
	pause bool
	// The speed of the playback.
	// The same applies here (see above).
	speed float64
}

// The representation of a client on the server-side.
type serverClient struct {
	// The message stream of the client.
	stream network.Stream
	// Whether the client is ready.
	ready bool
}

// Fileserver for serving local media.
type fileServer struct {
	// The mutex used to protect path.
	mu sync.RWMutex
	// The local path to serve.
	path string
}

// Command for new connections.
type cmdClientConnected struct {
	// The message stream of the incoming connection.
	stream network.Stream
}

// Command to mark when a client is ready.
type cmdClientReady struct {
	// The client which is ready.
	client *serverClient
}

// Command for actions taken by a client.
type cmdClientAction struct {
	// The client which took the action.
	id int
	// The message type of the action.
	typ messageType
	// The payload of the action.
	payload any
}

// Command for when a client disconnects.
type cmdClientDisconnected struct {
	// The ID if the disconnected client.
	id int
	// The client which disconnected.
	client *serverClient
}

func runServer(args []string) {
	// NOTE: input
	if len(args) == 0 {
		path := readLine("Path/URL: ")
		args = []string{path}
	}

	// NOTE: trap setup
	TrapRegister()
	defer TrapRun()

	// NOTE: server data
	s := server{
		debouncer: newDebouncer(),
		cmdChan:   make(chan any),
		clients:   make(map[int]*serverClient),
	}

	// NOTE: P2P setup
	h, idht := createHost()
	TrapDefer(func() {
		h.Close()
	})

	disc   := routing.NewRoutingDiscovery(idht)
	phrase := generatePhrase()

	_, err := disc.Advertise(context.Background(), phrase)
	if err != nil { Fatal("Failed to advertise room:", err) }
	Println("Phrase: ", ansi.Bold, phrase, ansi.Reset)

	err = clipboard.Init()
	if err == nil {
		clipboardChan := clipboard.Write(clipboard.FmtText, []byte(phrase))
		if clipboardChan != nil {
			Note("Copied to clipboard")
		}
	}

	// NOTE: message handler
	h.SetStreamHandler(messageProtocol, func(stream network.Stream) {
		s.cmdChan <- cmdClientConnected{stream}
	})

	// NOTE: mpv
	setupPath()

	// IMPORTANT: imprecise seeks may result in multiple seconds of desync,
	// since what would be the precise seek is reported anyway
	s.mpv, s.ipc = mpv(append(args, "--hr-seek", "--quiet")...)
	TrapDefer(func() {
		s.mpv.Process.Kill()
	})
	go func() {
		s.mpv.Wait()
		TrapExit(0)
	}()

	loadedChan, _, err := s.ipc.Subscribe("file-loaded")
	if err != nil { Fatal("Failed to subscribe to event:", err) }

	restartChan, _, err := s.ipc.Subscribe("playback-restart")
	if err != nil { Fatal("Failed to subscribe to event:", err) }

	seekChan, _, err := s.ipc.Subscribe("seek")
	if err != nil { Fatal("Failed to subscribe to event:", err) }

	pauseChan, _, err := s.ipc.ObserveProperty("pause")
	if err != nil { Error("Failed to observe property:", err) }

	speedChan, _, err := s.ipc.ObserveProperty("speed")
	if err != nil { Error("Failed to observe property:", err) }

	for {
		select {
		case cmd := <-s.cmdChan:
			switch cmd := cmd.(type) {
			case cmdClientConnected:
				s.waitCount += 1
				s.waitStart()

				var time float64
				timeAny := <-s.ipc.Request("get_property", "playback-time")
				if err, ok := timeAny.(error); ok {
					Error("Failed to get playback time:", err)
				} else {
					time = timeAny.(float64)
				}

				sendMessage(cmd.stream, messageTypePlay, messagePlay{s.url, s.title, time, s.speed})

				id := s.clientID
				s.clientID += 1

				client := &serverClient{
					stream: cmd.stream,
					ready:  false,
				}
				s.clients[id] = client

				go s.serve(id, client)
				Notef("Client %d connected", id)

			case cmdClientReady:
				if !cmd.client.ready {
					cmd.client.ready = true
					s.waitCount -= 1
					s.waitCheck()
				}

			case cmdClientAction:
				if s.waiting {
					if cmd.typ == messageTypeSpeed {
						s.debouncer.push(messageTypeSpeed)
						s.ipc.Request("set_property", "speed", cmd.payload.(float64))

						for id, c := range s.clients {
							if id != cmd.id {
								sendMessage(c.stream, cmd.typ, cmd.payload)
							}
						}
					}
				} else {
					switch cmd.typ {
					case messageTypePause:
						s.debouncer.push(messageTypePause)
						s.ipc.Request("set_property", "pause", true)
					case messageTypeResume:
						s.debouncer.push(messageTypeResume)
						s.ipc.Request("set_property", "pause", false)
					case messageTypeSeek:
						s.waitAll()
						s.debouncer.push(messageTypeSeek)
						s.ipc.Request("set_property", "playback-time", cmd.payload.(float64))
					case messageTypeSpeed:
						s.debouncer.push(messageTypeSpeed)
						s.ipc.Request("set_property", "speed", cmd.payload.(float64))
					}

					for id, c := range s.clients {
						if id != cmd.id {
							sendMessage(c.stream, cmd.typ, cmd.payload)
						}
					}
				}

			case cmdClientDisconnected:
				if !cmd.client.ready {
					s.waitCount -= 1
					s.waitCheck()
				}

				delete(s.clients, cmd.id)
				Notef("Client %d disconnected", cmd.id)
			}

		case <-loadedChan:
			if len(s.clients) > 0 {
				s.waitAll()
			}

			rawPathAny := <-s.ipc.Request("get_property", "path")
			if err, ok := rawPathAny.(error); ok { Fatal("Failed to get path:", err) }
			rawPath := rawPathAny.(string)

			var protocol, path string
			components := strings.Split(rawPath, "://")
			if len(components) > 1 {
				protocol = components[0]
				path     = components[1]
			} else {
				protocol = "file"
				path     = components[0]
			}

			switch protocol {
			case "file":
				if s.fileServer != nil {
					s.fileServer.mu.Lock()
					s.fileServer.path = path
					s.fileServer.mu.Unlock()
				} else {
					s.fileServer = &fileServer{path: path}

					go func() {
						listener, err := gostream.Listen(h, streamProtocol)
						if err != nil { Fatal("Failed to listen for stream connections:", err) }

						server := http.Server{Handler: s.fileServer}
						err = server.Serve(listener)
						if err != nil && !errors.Is(err, http.ErrServerClosed) { Fatal("File server failed:", err) }
					}()
				}

				titleAny := <-s.ipc.Request("get_property", "media-title")
				if err, ok := titleAny.(error); ok {
					Error("Failed to get title:", err)
				} else {
					s.title = titleAny.(string)
				}
				s.url = ""

			case "http", "https":
				if s.fileServer != nil {
					s.fileServer.mu.Lock()
					s.fileServer.path = ""
					s.fileServer.mu.Unlock()
				}

				s.title = ""
				s.url   = rawPath

			default:
				Fatal("Unsupported protocol:", protocol)
			}

			if len(s.clients) > 0 {
				var time float64
				timeAny := <-s.ipc.Request("get_property", "playback-time")
				if err, ok := timeAny.(error); ok {
					Error("Failed to get playback time:", err)
				} else {
					time = timeAny.(float64)
				}

				msg := messagePlay{s.url, s.title, time, s.speed}
				for _, c := range s.clients {
					sendMessage(c.stream, messageTypePlay, msg)
				}
			}

		case <-restartChan:
			s.ready = true
			s.waitCheck()

		case <-seekChan:
			if s.debouncer.pop(messageTypeSeek) {
				break
			}

			if s.waiting {
				s.debouncer.push(messageTypeSeek)
				s.ipc.Request("set_property", "playback-time", s.waitTime)
				break
			}

			if len(s.clients) <= 0 {
				break
			}

			s.waitAll()

			timeAny := <-s.ipc.Request("get_property", "playback-time")
			if err, ok := timeAny.(error); ok {
				Error("Failed to get playback time:", err)
				break
			}
			time := timeAny.(float64)

			for _, c := range s.clients {
				sendMessage(c.stream, messageTypeSeek, time)
			}

		case pauseAny := <-pauseChan:
			pause := pauseAny.(bool)

			var typ messageType
			if pause {
				typ = messageTypePause
			} else {
				typ = messageTypeResume
			}

			if s.debouncer.pop(typ) {
				break
			}

			if s.waiting {
				if !pause {
					s.pause = true
					s.debouncer.push(messageTypePause)
					s.ipc.Request("set_property", "pause", true)

					s.debouncer.push(messageTypeSeek)
					s.ipc.Request("set_property", "playback-time", s.waitTime)
				}

				break
			}

			s.pause = pause

			for _, c := range s.clients {
				sendMessage(c.stream, typ, nil)
			}

		case speedAny := <-speedChan:
			if s.debouncer.pop(messageTypeSpeed) {
				break
			}

			s.speed = speedAny.(float64)

			for _, c := range s.clients {
				sendMessage(c.stream, messageTypeSpeed, s.speed)
			}
		}
	}
}

func (s *fileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	path := s.path
	s.mu.RUnlock()

	if path != "" {
		http.ServeFile(w, r, path)
	} else {
		http.Error(w, "no local media is playing", http.StatusServiceUnavailable)
	}
}

func (s *server) serve(id int, c *serverClient) {
	var data [4096]byte
	for {
		typ, payload, err := waitMessage(c.stream, data[:])
		if err != nil { break }

		forward := false
		switch typ {
		case messageTypeReady:
			s.cmdChan <- cmdClientReady{c}

		case messageTypePause:  forward = true
		case messageTypeResume: forward = true
		case messageTypeSeek:   forward = true
		case messageTypeSpeed:  forward = true
		}

		if forward {
			s.cmdChan <- cmdClientAction{id, typ, payload}
		}
	}

	c.stream.Close()

	s.cmdChan <- cmdClientDisconnected{id, c}
}

func (s *server) waitStart() {
	if !s.waiting {
		s.waiting    = true
		s.waitResume = !s.pause

		if s.waitResume {
			s.pause = true
			s.debouncer.push(messageTypePause)
			s.ipc.Request("set_property", "pause", true)
		}

		timeAny := <-s.ipc.Request("get_property", "playback-time")
		if err, ok := timeAny.(error); ok {
			Error("Failed to get playback time:", err)
		} else {
			s.waitTime = timeAny.(float64)
		}

		for _, c := range s.clients {
			sendMessage(c.stream, messageTypeWaitStart, s.waitTime)
		}
	}
}

func (s *server) waitAll() {
	s.ready = false
	for _, c := range s.clients {
		c.ready = false
	}

	s.waitCount = len(s.clients)
	s.waitStart()
}

func (s *server) waitCheck() {
	if s.waiting && s.ready && s.waitCount <= 0 {
		s.waiting = false

		if s.waitResume {
			s.pause = false
			s.debouncer.push(messageTypeResume)
			s.ipc.Request("set_property", "pause", false)
		}

		for _, c := range s.clients {
			sendMessage(c.stream, messageTypeWaitDone, s.waitResume)
		}
	}
}

// Taken from https://github.com/schollz/croc/blob/main/src/mnemonicode/wordlist.go
const (
	phraseSize      = 4
	phraseSeparator = "-"
)

var phraseWords = []string{
	"academy", "acrobat", "active", "actor", "adam", "admiral",
	"adrian", "africa", "agenda", "agent", "airline", "airport",
	"aladdin", "alarm", "alaska", "albert", "albino", "album",
	"alcohol", "alex", "algebra", "alibi", "alice", "alien",
	"alpha", "alpine", "amadeus", "amanda", "amazon", "amber",
	"america", "amigo", "analog", "anatomy", "angel", "animal",
	"antenna", "antonio", "apollo", "april", "archive", "arctic",
	"arizona", "arnold", "aroma", "arthur", "artist", "asia",
	"aspect", "aspirin", "athena", "athlete", "atlas", "audio",
	"august", "austria", "axiom", "aztec", "balance", "ballad",
	"banana", "bandit", "banjo", "barcode", "baron", "basic",
	"battery", "belgium", "berlin", "bermuda", "bernard", "bikini",
	"binary", "bingo", "biology", "block", "blonde", "bonus",
	"boris", "boston", "boxer", "brandy", "bravo", "brazil",
	"bronze", "brown", "bruce", "bruno", "burger", "burma",
	"cabinet", "cactus", "cafe", "cairo", "cake", "calypso",
	"camel", "camera", "campus", "canada", "canal", "cannon",
	"canoe", "cantina", "canvas", "canyon", "capital", "caramel",
	"caravan", "carbon", "cargo", "carlo", "carol", "carpet",
	"cartel", "casino", "castle", "castro", "catalog", "caviar",
	"cecilia", "cement", "center", "century", "ceramic", "chamber",
	"chance", "change", "chaos", "charlie", "charm", "charter",
	"chef", "chemist", "cherry", "chess", "chicago", "chicken",
	"chief", "china", "cigar", "cinema", "circus", "citizen",
	"city", "clara", "classic", "claudia", "clean", "client",
	"climax", "clinic", "clock", "club", "cobra", "coconut",
	"cola", "collect", "colombo", "colony", "color", "combat",
	"comedy", "comet", "command", "compact", "company", "complex",
	"concept", "concert", "connect", "consul", "contact", "context",
	"contour", "control", "convert", "copy", "corner", "corona",
	"correct", "cosmos", "couple", "courage", "cowboy", "craft",
	"crash", "credit", "cricket", "critic", "crown", "crystal",
	"cuba", "culture", "dallas", "dance", "daniel", "david",
	"decade", "decimal", "deliver", "delta", "deluxe", "demand",
	"demo", "denmark", "derby", "design", "detect", "develop",
	"diagram", "dialog", "diamond", "diana", "diego", "diesel",
	"diet", "digital", "dilemma", "diploma", "direct", "disco",
	"disney", "distant", "doctor", "dollar", "dominic", "domino",
	"donald", "dragon", "drama", "dublin", "duet", "dynamic",
	"east", "ecology", "economy", "edgar", "egypt", "elastic",
	"elegant", "element", "elite", "elvis", "email", "energy",
	"engine", "english", "episode", "equator", "escort", "ethnic",
	"europe", "everest", "evident", "exact", "example", "exit",
	"exotic", "export", "express", "extra", "fabric", "factor",
	"falcon", "family", "fantasy", "fashion", "fiber", "fiction",
	"fidel", "fiesta", "figure", "film", "filter", "final",
	"finance", "finish", "finland", "flash", "florida", "flower",
	"fluid", "flute", "focus", "ford", "forest", "formal",
	"format", "formula", "fortune", "forum", "fragile", "france",
	"frank", "friend", "frozen", "future", "gabriel", "galaxy",
	"gallery", "gamma", "garage", "garden", "garlic", "gemini",
	"general", "genetic", "genius", "germany", "global", "gloria",
	"golf", "gondola", "gong", "good", "gordon", "gorilla",
	"grand", "granite", "graph", "green", "group", "guide",
	"guitar", "guru", "hand", "happy", "harbor", "harmony",
	"harvard", "havana", "hawaii", "helena", "hello", "henry",
	"hilton", "history", "horizon", "hotel", "human", "humor",
	"icon", "idea", "igloo", "igor", "image", "impact",
	"import", "index", "india", "indigo", "input", "insect",
	"instant", "iris", "italian", "jacket", "jacob", "jaguar",
	"janet", "japan", "jargon", "jazz", "jeep", "john",
	"joker", "jordan", "jumbo", "june", "jungle", "junior",
	"jupiter", "karate", "karma", "kayak", "kermit", "kilo",
	"king", "koala", "korea", "labor", "lady", "lagoon",
	"laptop", "laser", "latin", "lava", "lecture", "left",
	"legal", "lemon", "level", "lexicon", "liberal", "libra",
	"limbo", "limit", "linda", "linear", "lion", "liquid",
	"liter", "little", "llama", "lobby", "lobster", "local",
	"logic", "logo", "lola", "london", "lotus", "lucas",
	"lunar", "machine", "macro", "madam", "madonna", "madrid",
	"maestro", "magic", "magnet", "magnum", "major", "mama",
	"mambo", "manager", "mango", "manila", "marco", "marina",
	"market", "mars", "martin", "marvin", "master", "matrix",
	"maximum", "media", "medical", "mega", "melody", "melon",
	"memo", "mental", "mentor", "menu", "mercury", "message",
	"metal", "meteor", "meter", "method", "metro", "mexico",
	"miami", "micro", "million", "mineral", "minimum", "minus",
	"minute", "miracle", "mirage", "miranda", "mister", "mixer",
	"mobile", "model", "modem", "modern", "modular", "moment",
	"monaco", "monica", "monitor", "mono", "monster", "montana",
	"morgan", "motel", "motif", "motor", "mozart", "multi",
	"museum", "music", "mustang", "natural", "neon", "nepal",
	"neptune", "nerve", "neutral", "nevada", "news", "ninja",
	"nirvana", "normal", "nova", "novel", "nuclear", "numeric",
	"nylon", "oasis", "object", "observe", "ocean", "octopus",
	"olivia", "olympic", "omega", "opera", "optic", "optimal",
	"orange", "orbit", "organic", "orient", "origin", "orlando",
	"oscar", "oxford", "oxygen", "ozone", "pablo", "pacific",
	"pagoda", "palace", "pamela", "panama", "panda", "panel",
	"panic", "paradox", "pardon", "paris", "parker", "parking",
	"parody", "partner", "passage", "passive", "pasta", "pastel",
	"patent", "patriot", "patrol", "patron", "pegasus", "pelican",
	"penguin", "pepper", "percent", "perfect", "perfume", "period",
	"permit", "person", "peru", "phone", "photo", "piano",
	"picasso", "picnic", "picture", "pigment", "pilgrim", "pilot",
	"pirate", "pixel", "pizza", "planet", "plasma", "plaster",
	"plastic", "plaza", "pocket", "poem", "poetic", "poker",
	"polaris", "police", "politic", "polo", "polygon", "pony",
	"popcorn", "popular", "postage", "postal", "precise", "prefix",
	"premium", "present", "price", "prince", "printer", "prism",
	"private", "product", "profile", "program", "project", "protect",
	"proton", "public", "pulse", "puma", "pyramid", "queen",
	"radar", "radio", "random", "rapid", "rebel", "record",
	"recycle", "reflex", "reform", "regard", "regular", "relax",
	"report", "reptile", "reverse", "ricardo", "ringo", "ritual",
	"robert", "robot", "rocket", "rodeo", "romeo", "royal",
	"russian", "safari", "salad", "salami", "salmon", "salon",
	"salute", "samba", "sandra", "santana", "sardine", "school",
	"screen", "script", "second", "secret", "section", "segment",
	"select", "seminar", "senator", "senior", "sensor", "serial",
	"service", "sheriff", "shock", "sierra", "signal", "silicon",
	"silver", "similar", "simon", "single", "siren", "slogan",
	"social", "soda", "solar", "solid", "solo", "sonic",
	"soviet", "special", "speed", "spiral", "spirit", "sport",
	"static", "station", "status", "stereo", "stone", "stop",
	"street", "strong", "student", "studio", "style", "subject",
	"sultan", "super", "susan", "sushi", "suzuki", "switch",
	"symbol", "system", "tactic", "tahiti", "talent", "tango",
	"tarzan", "taxi", "telex", "tempo", "tennis", "texas",
	"textile", "theory", "thermos", "tiger", "titanic", "tokyo",
	"tomato", "topic", "tornado", "toronto", "torpedo", "total",
	"totem", "tourist", "tractor", "traffic", "transit", "trapeze",
	"travel", "tribal", "trick", "trident", "trilogy", "tripod",
	"tropic", "trumpet", "tulip", "tuna", "turbo", "twist",
	"ultra", "uniform", "union", "uranium", "vacuum", "valid",
	"vampire", "vanilla", "vatican", "velvet", "ventura", "venus",
	"vertigo", "veteran", "victor", "video", "vienna", "viking",
	"village", "vincent", "violet", "violin", "virtual", "virus",
	"visa", "vision", "visitor", "visual", "vitamin", "viva",
	"vocal", "vodka", "volcano", "voltage", "volume", "voyage",
	"water", "weekend", "welcome", "western", "window", "winter",
	"wizard", "wolf", "world", "xray", "yankee", "yoga",
	"yogurt", "yoyo", "zebra", "zero", "zigzag", "zipper",
	"zodiac", "zoom", "abraham", "action", "address", "alabama",
	"alfred", "almond", "ammonia", "analyze", "annual", "answer",
	"apple", "arena", "armada", "arsenal", "atlanta", "atomic",
	"avenue", "average", "bagel", "baker", "ballet", "bambino",
	"bamboo", "barbara", "basket", "bazaar", "benefit", "bicycle",
	"bishop", "blitz", "bonjour", "bottle", "bridge", "british",
	"brother", "brush", "budget", "cabaret", "cadet", "candle",
	"capitan", "capsule", "career", "cartoon", "channel", "chapter",
	"cheese", "circle", "cobalt", "cockpit", "college", "compass",
	"comrade", "condor", "crimson", "cyclone", "darwin", "declare",
	"degree", "delete", "delphi", "denver", "desert", "divide",
	"dolby", "domain", "domingo", "double", "drink", "driver",
	"eagle", "earth", "echo", "eclipse", "editor", "educate",
	"edward", "effect", "electra", "emerald", "emotion", "empire",
	"empty", "escape", "eternal", "evening", "exhibit", "expand",
	"explore", "extreme", "ferrari", "first", "flag", "folio",
	"forget", "forward", "freedom", "fresh", "friday", "fuji",
	"galileo", "garcia", "genesis", "gold", "gravity", "habitat",
	"hamlet", "harlem", "helium", "holiday", "house", "hunter",
	"ibiza", "iceberg", "imagine", "infant", "isotope", "jackson",
	"jamaica", "jasmine", "java", "jessica", "judo", "kitchen",
	"lazarus", "letter", "license", "lithium", "loyal", "lucky",
	"magenta", "mailbox", "manual", "marble", "mary", "maxwell",
	"mayor", "milk", "monarch", "monday", "money", "morning",
	"mother", "mystery", "native", "nectar", "nelson", "network",
	"next", "nikita", "nobel", "nobody", "nominal", "norway",
	"nothing", "number", "october", "office", "oliver", "opinion",
	"option", "order", "outside", "package", "pancake", "pandora",
	"panther", "papa", "patient", "pattern", "pedro", "pencil",
	"people", "phantom", "philips", "pioneer", "pluto", "podium",
	"portal", "potato", "prize", "process", "protein", "proxy",
	"pump", "pupil", "python", "quality", "quarter", "quiet",
	"rabbit", "radical", "radius", "rainbow", "ralph", "ramirez",
	"ravioli", "raymond", "respect", "respond", "result", "resume",
	"retro", "richard", "right", "risk", "river", "roger",
	"roman", "rondo", "sabrina", "salary", "salsa", "sample",
	"samuel", "saturn", "savage", "scarlet", "scoop", "scorpio",
	"scratch", "scroll", "sector", "serpent", "shadow", "shampoo",
	"sharon", "sharp", "short", "shrink", "silence", "silk",
	"simple", "slang", "smart", "smoke", "snake", "society",
	"sonar", "sonata", "soprano", "source", "sparta", "sphere",
	"spider", "sponsor", "spring", "acid", "adios", "agatha",
	"alamo", "alert", "almanac", "aloha", "andrea", "anita",
	"arcade", "aurora", "avalon", "baby", "baggage", "balloon",
	"bank", "basil", "begin", "biscuit", "blue", "bombay",
	"brain", "brenda", "brigade", "cable", "carmen", "cello",
	"celtic", "chariot", "chrome", "citrus", "civil", "cloud",
	"common", "compare", "cool", "copper", "coral", "crater",
	"cubic", "cupid", "cycle", "depend", "door", "dream",
	"dynasty", "edison", "edition", "enigma", "equal", "eric",
	"event", "evita", "exodus", "extend", "famous", "farmer",
	"food", "fossil", "frog", "fruit", "geneva", "gentle",
	"george", "giant", "gilbert", "gossip", "gram", "greek",
	"grille", "hammer", "harvest", "hazard", "heaven", "herbert",
	"heroic", "hexagon", "husband", "immune", "inca", "inch",
	"initial", "isabel", "ivory", "jason", "jerome", "joel",
	"joshua", "journal", "judge", "juliet", "jump", "justice",
	"kimono", "kinetic", "leonid", "lima", "maze", "medusa",
	"member", "memphis", "michael", "miguel", "milan", "mile",
	"miller", "mimic", "mimosa", "mission", "monkey", "moral",
	"moses", "mouse", "nancy", "natasha", "nebula", "nickel",
	"nina", "noise", "orchid", "oregano", "origami", "orinoco",
	"orion", "othello", "paper", "paprika", "prelude", "prepare",
	"pretend", "profit", "promise", "provide", "puzzle", "remote",
	"repair", "reply", "rival", "riviera", "robin", "rose",
	"rover", "rudolf", "saga", "sahara", "scholar", "shelter",
	"ship", "shoe", "sigma", "sister", "sleep", "smile",
	"spain", "spark", "split", "spray", "square", "stadium",
	"star", "storm", "story", "strange", "stretch", "stuart",
	"subway", "sugar", "sulfur", "summer", "survive", "sweet",
	"swim", "table", "taboo", "target", "teacher", "telecom",
	"temple", "tibet", "ticket", "tina", "today", "toga",
	"tommy", "tower", "trivial", "tunnel", "turtle", "twin",
	"uncle", "unicorn", "unique", "update", "valery", "vega",
	"version", "voodoo", "warning", "william", "wonder", "year",
	"yellow", "young", "absent", "absorb", "accent", "alfonso",
	"alias", "ambient", "andy", "anvil", "appear", "apropos",
	"archer", "ariel", "armor", "arrow", "austin", "avatar",
	"axis", "baboon", "bahama", "bali", "balsa", "bazooka",
	"beach", "beast", "beatles", "beauty", "before", "benny",
	"betty", "between", "beyond", "billy", "bison", "blast",
	"bless", "bogart", "bonanza", "book", "border", "brave",
	"bread", "break", "broken", "bucket", "buenos", "buffalo",
	"bundle", "button", "buzzer", "byte", "caesar", "camilla",
	"canary", "candid", "carrot", "cave", "chant", "child",
	"choice", "chris", "cipher", "clarion", "clark", "clever",
	"cliff", "clone", "conan", "conduct", "congo", "content",
	"costume", "cotton", "cover", "crack", "current", "danube",
	"data", "decide", "desire", "detail", "dexter", "dinner",
	"dispute", "donor", "druid", "drum", "easy", "eddie",
	"enjoy", "enrico", "epoxy", "erosion", "except", "exile",
	"explain", "fame", "fast", "father", "felix", "field",
	"fiona", "fire", "fish", "flame", "flex", "flipper",
	"float", "flood", "floor", "forbid", "forever", "fractal",
	"frame", "freddie", "front", "fuel", "gallop", "game",
	"garbo", "gate", "gibson", "ginger", "giraffe", "gizmo",
	"glass", "goblin", "gopher", "grace", "gray", "gregory",
	"grid", "griffin", "ground", "guest", "gustav", "gyro",
	"hair", "halt", "harris", "heart", "heavy", "herman",
	"hippie", "hobby", "honey", "hope", "horse", "hostel",
	"hydro", "imitate", "info", "ingrid", "inside", "invent",
	"invest", "invite", "iron", "ivan", "james", "jester",
	"jimmy", "join", "joseph", "juice", "julius", "july",
	"justin", "kansas", "karl", "kevin", "kiwi", "ladder",
	"lake", "laura", "learn", "legacy", "legend", "lesson",
	"life", "light", "list", "locate", "lopez", "lorenzo",
	"love", "lunch", "malta", "mammal", "margo", "marion",
	"mask", "match", "mayday", "meaning", "mercy", "middle",
	"mike", "mirror", "modest", "morph", "morris", "nadia",
	"nato", "navy", "needle", "neuron", "never", "newton",
	"nice", "night", "nissan", "nitro", "nixon", "north",
	"oberon", "octavia", "ohio", "olga", "open", "opus",
	"orca", "oval", "owner", "page", "paint", "palma",
	"parade", "parent", "parole", "paul", "peace", "pearl",
	"perform", "phoenix", "phrase", "pierre", "pinball", "place",
	"plate", "plato", "plume", "pogo", "point", "polite",
	"polka", "poncho", "powder", "prague", "press", "presto",
	"pretty", "prime", "promo", "quasi", "quest", "quick",
	"quiz", "quota", "race", "rachel", "raja", "ranger",
	"region", "remark", "rent", "reward", "rhino", "ribbon",
	"rider", "road", "rodent", "round", "rubber", "ruby",
	"rufus", "sabine", "saddle", "sailor", "saint", "salt",
	"satire", "scale", "scuba", "season", "secure", "shake",
	"shallow", "shannon", "shave", "shelf", "sherman", "shine",
	"shirt", "side", "sinatra", "sincere", "size", "slalom",
	"slow", "small", "snow", "sofia", "song", "sound",
	"south", "speech", "spell", "spend", "spoon", "stage",
	"stamp", "stand", "state", "stella", "stick", "sting",
	"stock", "store", "sunday", "sunset", "support", "sweden",
	"swing", "tape", "think", "thomas", "tictac", "time",
	"toast", "tobacco", "tonight", "torch", "torso", "touch",
	"toyota", "trade", "tribune", "trinity", "triton", "truck",
	"trust", "type", "under", "unit", "urban", "urgent",
	"user", "value", "vendor", "venice", "verona", "vibrate",
	"virgo", "visible", "vista", "vital", "voice", "vortex",
	"waiter", "watch", "wave", "weather", "wedding", "wheel",
	"whiskey", "wisdom", "deal", "null", "nurse", "quebec",
	"reserve", "reunion", "roof", "singer", "verbal", "amen",
	"ego", "fax", "jet", "job", "rio", "ski",
	"yes",
}

func generatePhrase() string {
	words := make([]string, phraseSize)
	for i := range words {
		words[i] = phraseWords[rand.Intn(len(phraseWords))]
	}

	return strings.Join(words, phraseSeparator)
}
