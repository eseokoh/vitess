# Generated by the protocol buffer compiler.  DO NOT EDIT!
# source: time.proto

import sys
_b=sys.version_info[0]<3 and (lambda x:x) or (lambda x:x.encode('latin1'))
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from google.protobuf import reflection as _reflection
from google.protobuf import symbol_database as _symbol_database
# @@protoc_insertion_point(imports)

_sym_db = _symbol_database.Default()




DESCRIPTOR = _descriptor.FileDescriptor(
  name='time.proto',
  package='vttime',
  syntax='proto3',
  serialized_options=_b('Z#vitess.io/vitess/go/vt/proto/vttime'),
  serialized_pb=_b('\n\ntime.proto\x12\x06vttime\",\n\x04Time\x12\x0f\n\x07seconds\x18\x01 \x01(\x03\x12\x13\n\x0bnanoseconds\x18\x02 \x01(\x05\x42%Z#vitess.io/vitess/go/vt/proto/vttimeb\x06proto3')
)




_TIME = _descriptor.Descriptor(
  name='Time',
  full_name='vttime.Time',
  filename=None,
  file=DESCRIPTOR,
  containing_type=None,
  fields=[
    _descriptor.FieldDescriptor(
      name='seconds', full_name='vttime.Time.seconds', index=0,
      number=1, type=3, cpp_type=2, label=1,
      has_default_value=False, default_value=0,
      message_type=None, enum_type=None, containing_type=None,
      is_extension=False, extension_scope=None,
      serialized_options=None, file=DESCRIPTOR),
    _descriptor.FieldDescriptor(
      name='nanoseconds', full_name='vttime.Time.nanoseconds', index=1,
      number=2, type=5, cpp_type=1, label=1,
      has_default_value=False, default_value=0,
      message_type=None, enum_type=None, containing_type=None,
      is_extension=False, extension_scope=None,
      serialized_options=None, file=DESCRIPTOR),
  ],
  extensions=[
  ],
  nested_types=[],
  enum_types=[
  ],
  serialized_options=None,
  is_extendable=False,
  syntax='proto3',
  extension_ranges=[],
  oneofs=[
  ],
  serialized_start=22,
  serialized_end=66,
)

DESCRIPTOR.message_types_by_name['Time'] = _TIME
_sym_db.RegisterFileDescriptor(DESCRIPTOR)

Time = _reflection.GeneratedProtocolMessageType('Time', (_message.Message,), dict(
  DESCRIPTOR = _TIME,
  __module__ = 'time_pb2'
  # @@protoc_insertion_point(class_scope:vttime.Time)
  ))
_sym_db.RegisterMessage(Time)


DESCRIPTOR._options = None
# @@protoc_insertion_point(module_scope)
