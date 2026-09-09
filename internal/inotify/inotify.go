// Package inotify é um wrapper fino sobre a API inotify(7) do Linux.
//
// Existe em vez de uma biblioteca pronta por um motivo específico: o campo
// `cookie` do evento, que é o único elo entre um IN_MOVED_FROM e o
// IN_MOVED_TO correspondente. Sem ele não há como distinguir um rename de um
// par delete+create — e rename/move é operação de primeira classe no escopo
// do Synkronyx. As bibliotecas usuais não expõem esse campo.
//
// Esta camada não interpreta nada: entrega o evento cru, com wd, máscara,
// cookie e nome. A tradução para o domínio é responsabilidade do
// internal/watcher.
package inotify

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Máscara padrão de interesse, derivada da seção 4 do escopo.
//
// IN_CLOSE_WRITE, e não IN_MODIFY, é o sinal de "conteúdo pronto": um write
// grande emite muitos IN_MODIFY e apenas um IN_CLOSE_WRITE, no fim.
// IN_MODIFY continua na máscara para dar sinal de vida durante escritas
// longas, mas quem dispara sincronização é o close.
const DefaultMask uint32 = unix.IN_CREATE |
	unix.IN_CLOSE_WRITE |
	unix.IN_MODIFY |
	unix.IN_DELETE |
	unix.IN_DELETE_SELF |
	unix.IN_MOVED_FROM |
	unix.IN_MOVED_TO |
	unix.IN_MOVE_SELF |
	unix.IN_ATTRIB |
	unix.IN_EXCL_UNLINK

// ErrClosed é devolvido por Read após Close.
var ErrClosed = errors.New("inotify: fechado")

// Event é um evento cru do inotify.
type Event struct {
	Wd     int32
	Mask   uint32
	Cookie uint32
	Name   string // nome da entrada dentro do diretório observado; vazio para eventos do próprio watch
}

// Is informa se a máscara do evento contém algum dos bits dados.
func (e Event) Is(bits uint32) bool { return e.Mask&bits != 0 }

// IsDir informa se o evento se refere a um diretório.
func (e Event) IsDir() bool { return e.Mask&unix.IN_ISDIR != 0 }

// Inotify é uma instância de inotify com desligamento cooperativo.
type Inotify struct {
	fd     int
	epfd   int
	wakefd int // eventfd usado para acordar o epoll_wait no Close

	mu     sync.Mutex
	closed bool

	buf [64 * 1024]byte // buffer de leitura; reaproveitado entre chamadas a Read
}

// New cria uma instância de inotify.
func New() (*Inotify, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("inotify_init1: %w", err)
	}

	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("epoll_create1: %w", err)
	}

	wakefd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		unix.Close(fd)
		unix.Close(epfd)
		return nil, fmt.Errorf("eventfd: %w", err)
	}

	in := &Inotify{fd: fd, epfd: epfd, wakefd: wakefd}
	for _, w := range []int{fd, wakefd} {
		ev := unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(w)}
		if err := unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, w, &ev); err != nil {
			in.closeFds()
			return nil, fmt.Errorf("epoll_ctl add %d: %w", w, err)
		}
	}
	return in, nil
}

// Add registra um watch em path e devolve o descritor (wd).
//
// Reregistrar um path já observado devolve o mesmo wd, com a máscara
// substituída — é assim que o kernel se comporta, e o chamador pode contar
// com isso para tornar o registro idempotente.
func (i *Inotify) Add(path string, mask uint32) (int32, error) {
	wd, err := unix.InotifyAddWatch(i.fd, path, mask)
	if err != nil {
		if errors.Is(err, unix.ENOSPC) {
			return 0, fmt.Errorf("watch %s: limite de watches do inotify atingido "+
				"(ajuste fs.inotify.max_user_watches): %w", path, err)
		}
		return 0, fmt.Errorf("watch %s: %w", path, err)
	}
	return int32(wd), nil
}

// Remove descarta um watch. Remover um wd já inválido não é erro: o kernel
// pode tê-lo invalidado sozinho (IN_IGNORED) antes de o chamador reagir.
func (i *Inotify) Remove(wd int32) error {
	if _, err := unix.InotifyRmWatch(i.fd, uint32(wd)); err != nil {
		if errors.Is(err, unix.EINVAL) {
			return nil
		}
		return fmt.Errorf("rm_watch %d: %w", wd, err)
	}
	return nil
}

// Read bloqueia até haver eventos e devolve o lote lido. Devolve ErrClosed
// depois de Close.
//
// Ler em lote não é otimização: o kernel entrega vários eventos por leitura e
// o pareamento MOVED_FROM/MOVED_TO frequentemente cabe dentro de um mesmo
// lote, o que o watcher aproveita.
func (i *Inotify) Read() ([]Event, error) {
	for {
		if i.isClosed() {
			return nil, ErrClosed
		}

		var epEvents [2]unix.EpollEvent
		n, err := unix.EpollWait(i.epfd, epEvents[:], -1)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return nil, fmt.Errorf("epoll_wait: %w", err)
		}
		if i.isClosed() {
			return nil, ErrClosed
		}

		var hasInotify bool
		for _, ev := range epEvents[:n] {
			if ev.Fd == int32(i.fd) {
				hasInotify = true
			}
		}
		if !hasInotify {
			continue
		}

		n, err = unix.Read(i.fd, i.buf[:])
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			return nil, fmt.Errorf("read: %w", err)
		}
		if n <= 0 {
			continue
		}
		return parse(i.buf[:n]), nil
	}
}

// parse decodifica o fluxo de structs inotify_event de tamanho variável.
func parse(buf []byte) []Event {
	const hdr = unix.SizeofInotifyEvent // 16 bytes

	var out []Event
	for off := 0; off+hdr <= len(buf); {
		raw := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
		nameLen := int(raw.Len)
		if off+hdr+nameLen > len(buf) {
			break // registro truncado: não deveria ocorrer, mas não vale arriscar
		}

		var name string
		if nameLen > 0 {
			nameBytes := buf[off+hdr : off+hdr+nameLen]
			// O kernel preenche com NULs até alinhar; o nome vai até o primeiro.
			if idx := indexByte(nameBytes, 0); idx >= 0 {
				nameBytes = nameBytes[:idx]
			}
			name = string(nameBytes)
		}

		out = append(out, Event{
			Wd:     raw.Wd,
			Mask:   raw.Mask,
			Cookie: raw.Cookie,
			Name:   name,
		})
		off += hdr + nameLen
	}
	return out
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// Close libera os descritores e desbloqueia um Read pendente.
func (i *Inotify) Close() error {
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return nil
	}
	i.closed = true
	i.mu.Unlock()

	// Acorda o epoll_wait antes de fechar qualquer fd, para que Read observe
	// o flag `closed` e saia sozinho em vez de ler de um fd fechado.
	var one [8]byte
	one[7] = 1
	if _, err := unix.Write(i.wakefd, one[:]); err != nil && !errors.Is(err, unix.EAGAIN) {
		return fmt.Errorf("acordando reader: %w", err)
	}
	return nil
}

// Shutdown fecha os descritores. Deve ser chamado depois de Close e de o
// leitor ter retornado.
func (i *Inotify) Shutdown() error { return i.closeFds() }

func (i *Inotify) closeFds() error {
	var errs []error
	for _, fd := range []*int{&i.fd, &i.epfd, &i.wakefd} {
		if *fd >= 0 {
			if err := unix.Close(*fd); err != nil && !errors.Is(err, os.ErrClosed) {
				errs = append(errs, err)
			}
			*fd = -1
		}
	}
	return errors.Join(errs...)
}

func (i *Inotify) isClosed() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.closed
}
