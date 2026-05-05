CC ?= cc
CFLAGS ?= -O2

all: libB.a

# parent.o here is the SUBDIRS collision twin of libA/parent.o.
# Same .o basename, different cwd, different VARIANT flag.
parent.o: bparent.c
	$(CC) $(CFLAGS) -DVARIANT=2 -I../include -c bparent.c -o parent.o

libB.a: parent.o
	ar rcs libB.a parent.o

clean:
	rm -f *.o *.a

.PHONY: all clean
