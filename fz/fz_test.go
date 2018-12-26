package fz

import (
	"bytes"
	"encoding/binary"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strconv"
	"testing"

	"github.com/coyove/common/rand"
)

const COUNT = 1 << 10

func genReader(r interface{}) io.Reader {
	buf := &bytes.Buffer{}
	switch x := r.(type) {
	case *rand.Rand:
		buf.Write(x.Fetch(8))
	case []byte:
		buf.Write(x)
	case int64:
		p := [8]byte{}
		binary.BigEndian.PutUint64(p[:], uint64(x))
		buf.Write(p[:])
	}
	return buf
}

var marker = []byte{1, 2, 3, 4, 5, 6, 7, 8}

func TestOpenFZ(t *testing.T) {
	os.Remove("test")
	f, err := OpenFZ("test", true)
	if f == nil {
		t.Fatal(err)
	}

	r := rand.New()
	for i := 0; i < COUNT; i++ {
		f.Put(uint128{r.Uint64(), r.Uint64()}, genReader(r))
		if i%100 == 0 {
			log.Println(i)
		}
	}

	f.Put(uint128{0, 13739}, genReader(marker))
	f.Close()

	f, err = OpenFZ("test", false)
	if f == nil {
		t.Fatal(err)
	}

	v, _ := f.Get(uint128{0, 13739})
	buf := v.ReadAllAndClose()
	if !bytes.Equal(buf, marker) {
		t.Error(buf)
	}

	f.Close()
}

func TestOpenFZ2(t *testing.T) {
	f, err := OpenFZ("map", true)
	if f == nil {
		t.Fatal(err)
	}

	for i := 0; i < 256; i++ {
		f.Put(uint128{0, uint64(i)}, genReader(int64(i)))
		if f.Count() != i+1 {
			t.Error("Count() failed")
		}
		for j := 0; j < i; j++ {
			v, _ := f.Get(uint128{0, uint64(j)})
			buf := v.ReadAllAndClose()
			vj := int64(binary.BigEndian.Uint64(buf))

			if vj != int64(j) {
				t.Error(vj, j)
			}
		}
	}

	f.Close()
	os.Remove("map")
}

func TestOpenFZ2Async(t *testing.T) {
	f, err := OpenFZ("map", true)
	if f == nil {
		t.Fatal(err)
	}

	f.SetFlag(LsAsyncCommit)

	for i := 0; i < COUNT; i++ {
		f.Put(uint128{0, uint64(i)}, genReader(int64(i)))
		if i%10 == 0 {
			f.Commit()
		}
		m := map[uint128]int64{}
		for j := 0; j <= i; j++ {
			v, _ := f.Get(uint128{0, uint64(j)})
			buf := v.ReadAllAndClose()
			m[v.key] = int64(binary.BigEndian.Uint64(buf))
		}
		f.Walk(func(key uint128, value *Data) error {
			v := int64(binary.BigEndian.Uint64(value.ReadAllAndClose()))
			if v != m[key] {
				t.Error(key, v, m[key], len(m))
			}
			delete(m, key)
			return nil
		})

		if len(m) > 0 {
			t.Error("We have a non-empty map")
		}
	}

	f.Commit()
	for j := 0; j < COUNT; j++ {
		v, _ := f.Get(uint128{0, uint64(j)})

		buf := v.ReadAllAndClose()
		vj := int64(binary.BigEndian.Uint64(buf))

		if vj != int64(j) {
			t.Error(vj, j)
		}
	}

	f.Close()
	os.Remove("map")
}

func BenchmarkFZ(b *testing.B) {
	f, err := OpenFZ("test", false)
	if f == nil {
		b.Fatal(err)
	}

	r := rand.New()
	for i := 0; i < b.N; i++ {
		f.Get(uint128{0, r.Uint64()})
	}

	f.Close()
}

func TestA_Begin(t *testing.T) {
	os.Mkdir("test2", 0777)
	rbuf := make([]byte, 8)

	for i := 0; i < COUNT; i++ {
		ioutil.WriteFile("test2/"+strconv.Itoa(i), rbuf, 0666)
	}
}

func BenchmarkFile(b *testing.B) {

	r := rand.New()
	for i := 0; i < b.N; i++ {
		f, _ := os.Open("test2/" + strconv.Itoa(r.Intn(COUNT)))
		buf := make([]byte, 8)
		f.Seek(0, 0)
		io.ReadAtLeast(f, buf, 8)
		f.Close()
	}

}

func BenchmarkZ_End(b *testing.B) {
	os.RemoveAll("test2")
}
