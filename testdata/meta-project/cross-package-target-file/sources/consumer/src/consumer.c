#include "consumer.h"
#include "producer.h"

int consumer_value(void) {
    return producer_value() + 1;
}
