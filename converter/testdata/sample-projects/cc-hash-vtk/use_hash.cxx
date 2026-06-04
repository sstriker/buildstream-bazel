// Consumes the generate-hash header: the macro expands to the digest string
// literal vtk_hash_source baked in. Returning it makes the symbol load-bearing
// at link, so the consumer build proves the cc_hash output is real.
#include "dataHash.h"

const char *get_data_hash()
{
  return dataHash;
}
