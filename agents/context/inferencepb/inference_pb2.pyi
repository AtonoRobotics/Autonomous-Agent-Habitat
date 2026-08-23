from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class Message(_message.Message):
    __slots__ = ("role", "content")
    ROLE_FIELD_NUMBER: _ClassVar[int]
    CONTENT_FIELD_NUMBER: _ClassVar[int]
    role: str
    content: str
    def __init__(self, role: _Optional[str] = ..., content: _Optional[str] = ...) -> None: ...

class CompleteRequest(_message.Message):
    __slots__ = ("provider", "providers", "model", "system", "messages", "max_tokens")
    PROVIDER_FIELD_NUMBER: _ClassVar[int]
    PROVIDERS_FIELD_NUMBER: _ClassVar[int]
    MODEL_FIELD_NUMBER: _ClassVar[int]
    SYSTEM_FIELD_NUMBER: _ClassVar[int]
    MESSAGES_FIELD_NUMBER: _ClassVar[int]
    MAX_TOKENS_FIELD_NUMBER: _ClassVar[int]
    provider: str
    providers: _containers.RepeatedScalarFieldContainer[str]
    model: str
    system: str
    messages: _containers.RepeatedCompositeFieldContainer[Message]
    max_tokens: int
    def __init__(self, provider: _Optional[str] = ..., providers: _Optional[_Iterable[str]] = ..., model: _Optional[str] = ..., system: _Optional[str] = ..., messages: _Optional[_Iterable[_Union[Message, _Mapping]]] = ..., max_tokens: _Optional[int] = ...) -> None: ...

class CompleteResponse(_message.Message):
    __slots__ = ("text", "input_tokens", "output_tokens")
    TEXT_FIELD_NUMBER: _ClassVar[int]
    INPUT_TOKENS_FIELD_NUMBER: _ClassVar[int]
    OUTPUT_TOKENS_FIELD_NUMBER: _ClassVar[int]
    text: str
    input_tokens: int
    output_tokens: int
    def __init__(self, text: _Optional[str] = ..., input_tokens: _Optional[int] = ..., output_tokens: _Optional[int] = ...) -> None: ...

class CountTokensResponse(_message.Message):
    __slots__ = ("input_tokens",)
    INPUT_TOKENS_FIELD_NUMBER: _ClassVar[int]
    input_tokens: int
    def __init__(self, input_tokens: _Optional[int] = ...) -> None: ...

class EmbedRequest(_message.Message):
    __slots__ = ("provider", "providers", "model", "input")
    PROVIDER_FIELD_NUMBER: _ClassVar[int]
    PROVIDERS_FIELD_NUMBER: _ClassVar[int]
    MODEL_FIELD_NUMBER: _ClassVar[int]
    INPUT_FIELD_NUMBER: _ClassVar[int]
    provider: str
    providers: _containers.RepeatedScalarFieldContainer[str]
    model: str
    input: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, provider: _Optional[str] = ..., providers: _Optional[_Iterable[str]] = ..., model: _Optional[str] = ..., input: _Optional[_Iterable[str]] = ...) -> None: ...

class EmbedFloatVector(_message.Message):
    __slots__ = ("values",)
    VALUES_FIELD_NUMBER: _ClassVar[int]
    values: _containers.RepeatedScalarFieldContainer[float]
    def __init__(self, values: _Optional[_Iterable[float]] = ...) -> None: ...

class EmbedResponse(_message.Message):
    __slots__ = ("embeddings", "dimension")
    EMBEDDINGS_FIELD_NUMBER: _ClassVar[int]
    DIMENSION_FIELD_NUMBER: _ClassVar[int]
    embeddings: _containers.RepeatedCompositeFieldContainer[EmbedFloatVector]
    dimension: int
    def __init__(self, embeddings: _Optional[_Iterable[_Union[EmbedFloatVector, _Mapping]]] = ..., dimension: _Optional[int] = ...) -> None: ...
