package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// Upgrader untuk mengubah koneksi HTTP menjadi WebSocket
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {return true},
}

// Struktur Data Pemain
type Player struct {
	ID int `json:"id"`
	Conn *websocket.Conn `json:"-"` // Tidak perlu di-serialize ke JSON
	Score int `json:"score"`
	Mistakes int `json:"mistakes"`
	Progress int `json:"progress"` // Persentase (0-100)
	IsDead bool `json:"is_dead"`
}

// Struktur Data Room / Sesi Game
type Room struct {
	RoomID string
	Players map[*websocket.Conn]*Player
	BaseBoard [9][9]int // Soal Sudoku awal (0 artinya kosong)
	CurrentState map[int]*[9][9]int // Lacak papan live masing-masing Player ID
	CorrectCount map[int]int // Mencatat jumlah kotak benar per Player ID
	Solution [9][9]int // Kunci jawaban untuk divalidasi di server
	TotalEmpty int // Total kotak kosong awal (untuk hitung progress)
	GameStarted bool
	Mu sync.Mutex
}

var (
	currentWaitingRoom *Room
	roomCounter int
	activeRooms = make(map[string]*Room)
	stateMu sync.Mutex
)

// Fungsi pembantu crypto-safe random number generator untuk menggantikan math/rand
func cryptoRandInt(max int) int {
	nBig, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return 0
	}
	return int(nBig.Int64())
}

// Fungsi pembantu untuk mengecek apakah angka aman ditaruh di koordinat tersebut
func isSafe(board *[9][9]int, row, col, num int) bool {
	// Cek baris
	for x := 0; x < 9; x++ {
		if board[row][x] == num {
			return false
		}
	}

	// Cek kolom
	for x := 0; x < 9; x++ {
		if board[x][col] == num {
			return false
		}
	}

	// Cek kotak blok 3x3
	startRow := row - row%3
	startCol := col - col%3
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			if board[i+startRow][j+startCol] == num {
				return false
			}
		}
	}
	return true
}

// Algoritma Backtracking untuk mengisi papan Sudoku secara penuh dan valid
func solveSudoku(board *[9][9]int) bool {
	row, col := -1, -1
	isEmpty := false

	for i := 0; i < 9; i++ {
		for j := 0; j < 9; j++ {
			if board[i][j] == 0 {
				row = i
				col = j
				isEmpty = true
				break
			}
		}
		if isEmpty {
			break
		}
	}

	if !isEmpty {
		return true // Papan sudah terisi penuh semua
	}

	// Coba masukkan angka 1-9 secara acak agar polanya bervariasi tiap game
	nums := [9]int{1, 2, 3, 4, 5, 6, 7, 8, 9}
	// Shuffle angka sederhana
	for i := range nums {
		j := cryptoRandInt(9)
		nums[i], nums[j] = nums[j], nums[i]
	}

	for _, num := range nums {
		if isSafe(board, row, col, num) {
			board[row][col] = num
			if solveSudoku(board) {
				return true
			}
			board[row][col] = 0 // Backtrack
		}
	}

	return false
}

// Fungsi generator sudoku otomatis
func generateSudoku() ([9][9]int, [9][9]int, int) {
	var sol[9][9]int

	// 1. Buat solusi penuh yang valid dulu
	solveSudoku(&sol)

	// 2. Salin solusi ke papan soal, lalu hapus beberapa kotak secara acak
	board := sol

	// Tentukan tingkat kesulitan (Berapa kotak kosong yang mau dibuat)
	// Untuk demo, set 30 kotak saja
	emptyCount := 30 + cryptoRandInt(16)

	// Hapus kotak secara deterministik/pseudo-random berdasarkan pola koordinat
	removed := 0
	for removed < emptyCount {
		r := cryptoRandInt(9)
		c := cryptoRandInt(9)
		if board[r][c] != 0 {
			board[r][c] = 0
			removed++
		}
	}

	return board, sol, removed
}

// Struktur JSON untuk pesan Broadcast dari Server ke semua Client
type BroadcastMessage struct {
	Type string `json:"type"` //"WAITING", "START", "UPDATE", "GAME_OVER"
	YourID int `json:"your_id,omitempty"`
	AllPlayers []*Player `json:"all_players"`
	Board *[9][9]int `json:"board,omitempty"`
	Message string `json:"message,omitempty"`
}

// Fungsi untuk mengirimkan status terbaru ke semua orang di dalam room
func broadcastToRoom(room *Room, msgType string, alertMsg string) {
	var playerList []*Player
	for _, p := range room.Players {
		playerList = append(playerList, p)
	}

	for conn, p := range room.Players {
		payload := BroadcastMessage{
			Type: msgType,
			YourID: p.ID,
			AllPlayers: playerList,
			Message: alertMsg,
		}
		if msgType == "START" {
			payload.Board = &room.BaseBoard
		}
		conn.WriteJSON(payload)
	}
}

// Cek sisa pemain yang masih hidup untuk auto-win
func checkRemainingPlayers(room *Room) {
	alivePlayers := []*Player{}
	var winner *Player

	for _, p := range room.Players {
		if !p.IsDead {
			alivePlayers = append(alivePlayers, p)
			winner = p
		}
	}

	// Ambil data list pemain terkini untuk broadcast terakhir
	var playerList []*Player
	for _, p := range room.Players {
		playerList = append(playerList, p)
	}

	// Jika hanya tersisa 1 pemain yang hidup, otomatis dia menang mutlak!
	if len(alivePlayers) == 1 && room.GameStarted {
		winner.Conn.WriteJSON(BroadcastMessage{Type: "GAME_OVER", AllPlayers: playerList, Message: "Semua lawan tereliminasi! Kamu Menang Mutlak!"})
		for conn, p := range room.Players {
			if p.ID != winner.ID {
				conn.WriteJSON(BroadcastMessage{Type: "GAME_OVER", AllPlayers: playerList, Message: fmt.Sprintf("Game Selesai! Player %d menang sebagai satu-satunya yang bertahan hidup.", winner.ID)})
			}
		}
	}
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	stateMu.Lock()
	// Jika belum ada lobi atau room penuh/sudah main, buat room baru
	if currentWaitingRoom == nil || len(currentWaitingRoom.Players) >= 4 || currentWaitingRoom.GameStarted {
		roomCounter++
		roomID := fmt.Sprintf("ROOM_%d", roomCounter)
		board, sol, emptyCount := generateSudoku()

		currentWaitingRoom = &Room{
			RoomID: roomID,
			Players: make(map[*websocket.Conn]*Player),
			CurrentState: make(map[int]*[9][9]int),
			CorrectCount: make(map[int]int),
			BaseBoard: board,
			Solution: sol,
			TotalEmpty: emptyCount,
		}
		activeRooms[roomID] = currentWaitingRoom
	}

	room := currentWaitingRoom
	pID := len(room.Players) + 1
	newPlayer := &Player{ID: pID, Conn: conn, Score: 0, Mistakes: 0, Progress: 0, IsDead: false}
	room.Players[conn] = newPlayer

	// Copy papan awal khusus untuk player ini
	playerBoard := room.BaseBoard
	room.CurrentState[pID] = &playerBoard
	room.CorrectCount[pID] = 0
	stateMu.Unlock()

	room.Mu.Lock()
	// Kabari semua orang di room kalau ada yang baru join
	broadcastToRoom(room, "WAITING", fmt.Sprintf("Pemain %d bergabung! Menunggu lawan... (%d/4)", pID, len(room.Players)))

	// Kalau sudah pas 4 orang, langsung gas mulai game otomatis
	if len(room.Players) == 4 && !room.GameStarted {
		room.GameStarted = true
		broadcastToRoom(room, "START", "Pertandingan dimulai! Selamat berlomba!")
	}
	room.Mu.Unlock()

	// Goroutine untuk mendengarkan ketikan pemain ini secara asinkron
	go listenToPlayer(room, newPlayer, conn)
}

type ClientInput struct {
	Action string `json:"action"` // "INPUT" atau "GIVE_UP"
	Row int `json:"row"`
	Col int `json:"col"`
	Value int `json:"value"`
}

func listenToPlayer(room *Room, me *Player, conn *websocket.Conn) {
	defer func() {
		conn.Close()
		room.Mu.Lock()
		delete(room.Players, conn)
		if len(room.Players) == 0 {
			stateMu.Lock()
			delete(activeRooms, room.RoomID)
			stateMu.Unlock()
		} else {
			broadcastToRoom(room, "UPDATE", fmt.Sprintf("Pemain %d keluar dari game.", me.ID))
			checkRemainingPlayers(room)
		}
		room.Mu.Unlock()
	}()

	for {
		_, msgBytes, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var input ClientInput
		if err := json.Unmarshal(msgBytes, &input); err != nil {continue}

		room.Mu.Lock()
		// Fitur mulai manual jika pemain sudah >= 2
		if input.Action == "FORCE_START" && !room.GameStarted && len(room.Players) >= 2 {
			room.GameStarted = true
			broadcastToRoom(room, "START", "Pertandingan dimulai lebih awal oleh pemain!")
			room.Mu.Unlock()
			continue
		}

		if !room.GameStarted || me.IsDead {
			room.Mu.Unlock()
			continue
		}

		if input.Action == "GIVE_UP" {
			me.IsDead = true
			conn.WriteJSON(BroadcastMessage{Type: "GAME_OVER", Message: "Kamu Menyerah!"})
			broadcastToRoom(room, "UPDATE", fmt.Sprintf("Pemain %d menyerah!", me.ID))
			checkRemainingPlayers(room)
			room.Mu.Unlock()
			continue
		}

		if input.Action == "INPUT" {
			pBoard := room.CurrentState[me.ID]

			// Jika kotak tersebut sudah dijawab BENAR sebelumnya, LOCK/Abaikan input baru
			if pBoard[input.Row][input.Col] == room.Solution[input.Row][input.Col] {
				room.Mu.Unlock()
				continue
			}

			// Cek jawaban (Server Authoritative)
			if room.Solution[input.Row][input.Col] == input.Value {
				pBoard[input.Row][input.Col] = input.Value
				me.Score += 10
				room.CorrectCount[me.ID]++
				// Rumus matematika persentase progress presisi tanpa loss bulat kebawah
				me.Progress = (room.CorrectCount[me.ID] * 100) / room.TotalEmpty
				if me.Progress > 100 {
					me.Progress = 100
				}

				conn.WriteJSON(map[string]interface{}{
					"type": "CORRECT_ANSWER",
					"row":  input.Row,
					"col":  input.Col,
					"val":  input.Value,
				})

				broadcastToRoom(room, "UPDATE", "")
			} else {
				me.Mistakes++
				if me.Score >= 50 {
					me.Score -= 50
				} else {
					me.Score = 0
				}

				conn.WriteJSON(BroadcastMessage{Type: "WRONG_ANSWER", Message: fmt.Sprintf("Angka %d salah pada baris %d, kolom %d!", input.Value, input.Row+1, input.Col+1)})
				broadcastToRoom(room, "UPDATE", "")
			}

			// Cek kalah (Nyawa habis = 3)
			if me.Mistakes >= 3 {
				me.IsDead = true
				conn.WriteJSON(BroadcastMessage{Type: "GAME_OVER", Message: "Nyawa habis! Kamu kalah!"})
				broadcastToRoom(room, "UPDATE", fmt.Sprintf("Pemain %d Tereliminasi!", me.ID))
				checkRemainingPlayers(room)
				room.Mu.Unlock()
				break
			}

			// Cek Menang (Progress 100%)
			if me.Progress >= 100 {
				broadcastToRoom(room, "GAME_OVER", fmt.Sprintf("Pemain %d menang! Berhasil menyelesaikan Sudoku duluan!", me.ID))
				room.Mu.Unlock()
				break
			}
		}
		room.Mu.Unlock()
	}
}

func main() {
	http.Handle("/", http.FileServer(http.Dir("./static")))
	http.HandleFunc("/ws", handleWebSocket)
	fmt.Println("Server 4-Player Sudoku jalan di http://localhost:8080")
	http.ListenAndServe(":8080", nil)
}