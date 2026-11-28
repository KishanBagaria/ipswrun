# The guests that hold the emulated kernel against a Mac's, a folder for each part of it: make,
# or make PLATFORM=ios, builds every folder's. See ../guests.txt for the list.
FOLDERS := $(patsubst %/Makefile,%,$(wildcard */Makefile))

all: $(FOLDERS)

$(FOLDERS):
	$(MAKE) -C $@

.PHONY: all $(FOLDERS)
