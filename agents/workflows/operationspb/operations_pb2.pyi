from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ProposeRequest(_message.Message):
    __slots__ = ("operation_id", "owner_extension_id", "effect_type", "payload_json", "reversibility", "retry_class")
    OPERATION_ID_FIELD_NUMBER: _ClassVar[int]
    OWNER_EXTENSION_ID_FIELD_NUMBER: _ClassVar[int]
    EFFECT_TYPE_FIELD_NUMBER: _ClassVar[int]
    PAYLOAD_JSON_FIELD_NUMBER: _ClassVar[int]
    REVERSIBILITY_FIELD_NUMBER: _ClassVar[int]
    RETRY_CLASS_FIELD_NUMBER: _ClassVar[int]
    operation_id: str
    owner_extension_id: str
    effect_type: str
    payload_json: str
    reversibility: str
    retry_class: str
    def __init__(self, operation_id: _Optional[str] = ..., owner_extension_id: _Optional[str] = ..., effect_type: _Optional[str] = ..., payload_json: _Optional[str] = ..., reversibility: _Optional[str] = ..., retry_class: _Optional[str] = ...) -> None: ...

class GetEffectRequest(_message.Message):
    __slots__ = ("effect_id",)
    EFFECT_ID_FIELD_NUMBER: _ClassVar[int]
    effect_id: str
    def __init__(self, effect_id: _Optional[str] = ...) -> None: ...

class ListEffectsByOperationRequest(_message.Message):
    __slots__ = ("operation_id",)
    OPERATION_ID_FIELD_NUMBER: _ClassVar[int]
    operation_id: str
    def __init__(self, operation_id: _Optional[str] = ...) -> None: ...

class ListEffectsByOperationResponse(_message.Message):
    __slots__ = ("effects",)
    EFFECTS_FIELD_NUMBER: _ClassVar[int]
    effects: _containers.RepeatedCompositeFieldContainer[Effect]
    def __init__(self, effects: _Optional[_Iterable[_Union[Effect, _Mapping]]] = ...) -> None: ...

class MarkDispatchPendingRequest(_message.Message):
    __slots__ = ("effect_id", "payload_json")
    EFFECT_ID_FIELD_NUMBER: _ClassVar[int]
    PAYLOAD_JSON_FIELD_NUMBER: _ClassVar[int]
    effect_id: str
    payload_json: str
    def __init__(self, effect_id: _Optional[str] = ..., payload_json: _Optional[str] = ...) -> None: ...

class MarkDispatchedRequest(_message.Message):
    __slots__ = ("effect_id", "external_command_id")
    EFFECT_ID_FIELD_NUMBER: _ClassVar[int]
    EXTERNAL_COMMAND_ID_FIELD_NUMBER: _ClassVar[int]
    effect_id: str
    external_command_id: str
    def __init__(self, effect_id: _Optional[str] = ..., external_command_id: _Optional[str] = ...) -> None: ...

class MarkObservedRequest(_message.Message):
    __slots__ = ("effect_id", "observation_ref", "observation_payload")
    EFFECT_ID_FIELD_NUMBER: _ClassVar[int]
    OBSERVATION_REF_FIELD_NUMBER: _ClassVar[int]
    OBSERVATION_PAYLOAD_FIELD_NUMBER: _ClassVar[int]
    effect_id: str
    observation_ref: str
    observation_payload: str
    def __init__(self, effect_id: _Optional[str] = ..., observation_ref: _Optional[str] = ..., observation_payload: _Optional[str] = ...) -> None: ...

class MarkOutcomeUnknownRequest(_message.Message):
    __slots__ = ("effect_id",)
    EFFECT_ID_FIELD_NUMBER: _ClassVar[int]
    effect_id: str
    def __init__(self, effect_id: _Optional[str] = ...) -> None: ...

class ResolveRequest(_message.Message):
    __slots__ = ("effect_id", "terminal", "error_code", "retryable", "message")
    EFFECT_ID_FIELD_NUMBER: _ClassVar[int]
    TERMINAL_FIELD_NUMBER: _ClassVar[int]
    ERROR_CODE_FIELD_NUMBER: _ClassVar[int]
    RETRYABLE_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    effect_id: str
    terminal: str
    error_code: str
    retryable: bool
    message: str
    def __init__(self, effect_id: _Optional[str] = ..., terminal: _Optional[str] = ..., error_code: _Optional[str] = ..., retryable: _Optional[bool] = ..., message: _Optional[str] = ...) -> None: ...

class Effect(_message.Message):
    __slots__ = ("effect_id", "operation_id", "owner_extension_id", "effect_type", "decision_id", "state", "forward_digest", "retry_class", "external_command_id", "observation_ref", "observation_payload", "error_code", "error_retryable", "error_message", "created_at", "updated_at")
    EFFECT_ID_FIELD_NUMBER: _ClassVar[int]
    OPERATION_ID_FIELD_NUMBER: _ClassVar[int]
    OWNER_EXTENSION_ID_FIELD_NUMBER: _ClassVar[int]
    EFFECT_TYPE_FIELD_NUMBER: _ClassVar[int]
    DECISION_ID_FIELD_NUMBER: _ClassVar[int]
    STATE_FIELD_NUMBER: _ClassVar[int]
    FORWARD_DIGEST_FIELD_NUMBER: _ClassVar[int]
    RETRY_CLASS_FIELD_NUMBER: _ClassVar[int]
    EXTERNAL_COMMAND_ID_FIELD_NUMBER: _ClassVar[int]
    OBSERVATION_REF_FIELD_NUMBER: _ClassVar[int]
    OBSERVATION_PAYLOAD_FIELD_NUMBER: _ClassVar[int]
    ERROR_CODE_FIELD_NUMBER: _ClassVar[int]
    ERROR_RETRYABLE_FIELD_NUMBER: _ClassVar[int]
    ERROR_MESSAGE_FIELD_NUMBER: _ClassVar[int]
    CREATED_AT_FIELD_NUMBER: _ClassVar[int]
    UPDATED_AT_FIELD_NUMBER: _ClassVar[int]
    effect_id: str
    operation_id: str
    owner_extension_id: str
    effect_type: str
    decision_id: str
    state: str
    forward_digest: str
    retry_class: str
    external_command_id: str
    observation_ref: str
    observation_payload: str
    error_code: str
    error_retryable: bool
    error_message: str
    created_at: str
    updated_at: str
    def __init__(self, effect_id: _Optional[str] = ..., operation_id: _Optional[str] = ..., owner_extension_id: _Optional[str] = ..., effect_type: _Optional[str] = ..., decision_id: _Optional[str] = ..., state: _Optional[str] = ..., forward_digest: _Optional[str] = ..., retry_class: _Optional[str] = ..., external_command_id: _Optional[str] = ..., observation_ref: _Optional[str] = ..., observation_payload: _Optional[str] = ..., error_code: _Optional[str] = ..., error_retryable: _Optional[bool] = ..., error_message: _Optional[str] = ..., created_at: _Optional[str] = ..., updated_at: _Optional[str] = ...) -> None: ...
