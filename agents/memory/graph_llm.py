"""Graphiti LLMClient/EmbedderClient adapters over the daemon's inference
seam (context.llm.ModelClient), per docs/AMH-SPECIFICATION.md's memory
architecture decision: semantic + entity memory are Graphiti/Neo4j
projections, and every model call an agent process makes — including ones
made on Graphiti's behalf — goes through the daemon's single-credential
inference seam (context/llm.py), never a direct provider SDK holding its
own key.

Graphiti's own concrete clients (OpenAIClient, AnthropicClient, ...) all
speak their provider's native wire format directly via that provider's
SDK. The daemon's /v1/inference/complete route is not that wire format —
it is AMH's own {provider, providers, model, system, messages, max_tokens}
-> {text} shape (see context/llm.py's docstring). So Graphiti is pointed
at the daemon by implementing its client interfaces directly against
ModelClient.complete()/.embed(), not by attempting base_url compatibility
with an OpenAI-shaped client.

ModelClient's HTTP calls are synchronous (urllib), while Graphiti's
client interfaces are async — bridged here with asyncio.to_thread rather
than introducing a second, async HTTP stack for the same daemon route.
"""

from __future__ import annotations

import asyncio
import json
from json import JSONDecodeError
from typing import Any

from graphiti_core.cross_encoder.client import CrossEncoderClient
from graphiti_core.embedder.client import EmbedderClient, EmbedderConfig
from graphiti_core.llm_client.client import LLMClient
from graphiti_core.llm_client.config import LLMConfig, ModelSize
from graphiti_core.llm_client.errors import RateLimitError
from graphiti_core.prompts.models import Message

from context.llm import ModelClient, ModelNotConfiguredError


def _extract_json_object(text: str) -> dict[str, Any]:
    """Extracts the first top-level JSON object from raw model text.

    The daemon's /v1/inference/complete route returns plain text, not a
    provider-native structured-output/tool-call result — so unlike
    Graphiti's OpenAI/Anthropic clients (which get JSON back directly from
    the provider's structured-output mechanism), the JSON object has to be
    located inside whatever prose the model wraps it in.
    """
    start = text.find("{")
    end = text.rfind("}") + 1
    if start < 0 or end <= start:
        raise JSONDecodeError(f"no JSON object found in model response: {text!r}", text, 0)
    return json.loads(text[start:end])


class DaemonGraphitiLLMClient(LLMClient):
    """Graphiti LLMClient backed by the daemon's inference seam.

    small_model, when given, is asked for on a ModelSize.small request
    (Graphiti uses this for cheaper/faster auxiliary calls like dedup and
    summarization) — otherwise every request uses the same model.
    """

    def __init__(self, model_client: ModelClient, small_model: str | None = None) -> None:
        super().__init__(config=LLMConfig(model=model_client.model, max_tokens=4096), cache=False)
        self._model_client = model_client
        self._small_model = small_model

    async def _generate_response(
        self,
        messages: list[Message],
        response_model: type[Any] | None = None,
        max_tokens: int = 4096,
        model_size: ModelSize = ModelSize.medium,
    ) -> dict[str, Any]:
        system = messages[0].content if messages and messages[0].role == "system" else ""
        rest = messages[1:] if messages and messages[0].role == "system" else messages
        chat_messages = [{"role": m.role, "content": m.content} for m in rest]

        requested_model = self._model_client.model
        if model_size == ModelSize.small and self._small_model:
            requested_model = self._small_model

        model_client = self._model_client
        if requested_model != model_client.model:
            model_client = ModelClient(
                daemon_api_base_url=model_client.daemon_api_base_url,
                agent_token=model_client.agent_token,
                model=requested_model,
                provider=model_client.provider,
                providers=model_client.providers,
                embedding_model=model_client.embedding_model,
                embedding_provider=model_client.embedding_provider,
                embedding_providers=model_client.embedding_providers,
            )

        try:
            text = await asyncio.to_thread(model_client.complete, system, chat_messages, max_tokens)
        except ModelNotConfiguredError as e:
            detail = str(e)
            if "HTTP 429" in detail or any(f"HTTP {code}" in detail for code in range(500, 600)):
                raise RateLimitError(detail) from e
            raise

        return _extract_json_object(text)


class DaemonGraphitiEmbedderClient(EmbedderClient):
    """Graphiti EmbedderClient backed by the daemon's inference seam.

    embedding_dim must match the real output dimensionality of the
    configured embedding model (model_client.embedding_model) — it sizes
    the graph store's vector index and is never guessed. Pass a smaller
    value only if the embedding model itself supports truncation
    (e.g. Matryoshka-trained models); this client truncates exactly like
    graphiti_core's own OpenAIEmbedder does, no more.
    """

    def __init__(self, model_client: ModelClient, embedding_dim: int) -> None:
        self.config = EmbedderConfig(embedding_dim=embedding_dim)
        self._model_client = model_client

    async def create(self, input_data) -> list[float]:
        text = input_data[0] if isinstance(input_data, list) else input_data
        vectors = await asyncio.to_thread(self._model_client.embed, [text])
        return vectors[0][: self.config.embedding_dim]

    async def create_batch(self, input_data_list: list[str]) -> list[list[float]]:
        vectors = await asyncio.to_thread(self._model_client.embed, input_data_list)
        return [v[: self.config.embedding_dim] for v in vectors]


_CROSS_ENCODER_SYSTEM = "You are an expert tasked with determining whether the passage is relevant to the query"
_CROSS_ENCODER_USER = (
    'Respond with only "True" if PASSAGE is relevant to QUERY and only "False" otherwise.\n'
    "<PASSAGE>\n{passage}\n</PASSAGE>\n<QUERY>\n{query}\n</QUERY>"
)


class DaemonGraphitiCrossEncoderClient(CrossEncoderClient):
    """Graphiti CrossEncoderClient backed by the daemon's inference seam.

    Graphiti's own OpenAIRerankerClient scores each passage from the raw
    token log-probability of a constrained True/False completion, which
    requires provider-native logprob access the daemon's plain-text
    /v1/inference/complete route does not expose. This does the same
    per-passage True/False classification, concurrently, and scores each
    passage 1.0/0.0 by the parsed verdict rather than a log-probability —
    a real classification per passage, just without sub-verdict
    confidence granularity.

    Only Graphiti's advanced `search_()` path with an explicit
    cross_encoder-reranking SearchConfig calls rank() at all; the plain
    `search()` this codebase uses (RRF / node-distance reranking) never
    does. This still has to exist because Graphiti's constructor requires
    a CrossEncoderClient and its own default (OpenAIRerankerClient) would
    hold a second, uncustodied provider credential.
    """

    def __init__(self, model_client: ModelClient) -> None:
        self._model_client = model_client

    async def rank(self, query: str, passages: list[str]) -> list[tuple[str, float]]:
        if not passages:
            return []

        async def score(passage: str) -> float:
            user = _CROSS_ENCODER_USER.format(passage=passage, query=query)
            text = await asyncio.to_thread(self._model_client.complete, _CROSS_ENCODER_SYSTEM, [{"role": "user", "content": user}], 5)
            return 1.0 if text.strip().lower().startswith("true") else 0.0

        scores = await asyncio.gather(*[score(p) for p in passages])
        return sorted(zip(passages, scores, strict=True), key=lambda pair: pair[1], reverse=True)
