// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Luiz Antonio Carlin

// Package preflight faz as verificações de ambiente que só têm resposta em
// tempo de execução.
//
// Todas são avisos, não erros: são condições em que o Synkronyx funciona, mas
// não faz exatamente o que a configuração promete. Descobrir isso na subida,
// no log, é muito melhor do que descobrir semanas depois ao notar que as
// permissões nunca foram propagadas.
package preflight

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// networkFilesystems são os tipos em que a numeração de usuários pode não
// corresponder à da máquina local. Em qualquer um deles, --numeric-ids
// preserva números que do outro lado significam outra pessoa.
var networkFilesystems = map[string]bool{
	"nfs":        true,
	"nfs4":       true,
	"cifs":       true,
	"smb3":       true,
	"smbfs":      true,
	"afs":        true,
	"9p":         true,
	"fuse.sshfs": true,
	"fuse.davfs": true,
	"ceph":       true,
	"glusterfs":  true,
}

// Mount descreve o ponto de montagem que contém um path.
type Mount struct {
	Point  string
	FSType string
}

// IsNetwork informa se o filesystem é de rede.
func (m Mount) IsNetwork() bool { return networkFilesystems[m.FSType] }

// MountOf devolve o ponto de montagem que contém path.
//
// A resposta é o ponto de montagem mais longo que seja prefixo do path —
// montagens aninhadas exigem isso, senão "/" venceria sempre.
func MountOf(path string) (Mount, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return Mount{}, fmt.Errorf("lendo mountinfo: %w", err)
	}
	defer f.Close()

	abs, err := filepath.Abs(path)
	if err != nil {
		return Mount{}, err
	}

	var best Mount
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// Formato: ID pai major:minor raiz ponto opções... - fstype fonte opções
		// O separador " - " marca o fim da parte de tamanho variável.
		line := sc.Text()
		sep := strings.Index(line, " - ")
		if sep < 0 {
			continue
		}

		left := strings.Fields(line[:sep])
		right := strings.Fields(line[sep+3:])
		if len(left) < 5 || len(right) < 1 {
			continue
		}

		point := unescapeOctal(left[4])
		if !isUnder(abs, point) {
			continue
		}
		if len(point) >= len(best.Point) {
			best = Mount{Point: point, FSType: right[0]}
		}
	}
	if err := sc.Err(); err != nil {
		return Mount{}, err
	}
	if best.Point == "" {
		return Mount{}, fmt.Errorf("nenhum ponto de montagem encontrado para %s", abs)
	}
	return best, nil
}

// unescapeOctal desfaz o escape que o kernel aplica em espaços e afins
// (\040, \011, \012, \134) nos campos do mountinfo.
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isUnder(path, dir string) bool {
	if dir == "/" {
		return true
	}
	return path == dir || strings.HasPrefix(path, dir+"/")
}

// PreservesOwnership informa se o processo consegue de fato definir dono e
// grupo dos arquivos que escreve.
//
// Sem isso, o -o/-g implicados por rsync --archive são pedidos que o kernel
// recusa: os arquivos ficam com o dono do processo. O rsync não falha por
// causa disso, o que torna o problema silencioso — e é justamente por ser
// silencioso que vale avisar.
func PreservesOwnership() bool { return os.Geteuid() == 0 }

// WantsOwnership informa se os argumentos de rsync pedem preservação de dono.
func WantsOwnership(args []string) bool {
	for _, a := range args {
		switch a {
		case "--archive", "-a", "--owner", "-o", "--group", "-g":
			return true
		}
		// Formas agrupadas como "-avz".
		if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsAny(a[1:], "aog") {
			return true
		}
	}
	return false
}

// InotifyLimit lê fs.inotify.max_user_watches.
func InotifyLimit() (int, error) {
	raw, err := os.ReadFile("/proc/sys/fs/inotify/max_user_watches")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("valor inesperado em max_user_watches: %w", err)
	}
	return n, nil
}

// WatchPressure é a fração do limite de watches já consumida por este
// processo. Devolve 0 se o limite não puder ser lido.
func WatchPressure(used int) float64 {
	limit, err := InotifyLimit()
	if err != nil || limit <= 0 {
		return 0
	}
	return float64(used) / float64(limit)
}
