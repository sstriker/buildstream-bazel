CC ?= cc
CFLAGS ?= -O2

all: libA.a

# parent.o is produced via -o into the cwd (libA/). The converter
# must key (cwd, "parent.o") to distinguish this compile from
# libB's parent.o.
parent.o: aparent.c
	$(CC) $(CFLAGS) -DVARIANT=1 -I../include -c aparent.c -o parent.o

libA.a: parent.o
	ar rcs libA.a parent.o

clean:
	rm -f *.o *.a

.PHONY: all clean
