package utils

import "hash/fnv"

type HashMethod func(v []byte) (h uint32)

func Fnv1a(buf []byte) (h uint32) {
	hash := fnv.New32a()
	hash.Write(buf)
	return hash.Sum32()
}
