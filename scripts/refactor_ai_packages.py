#!/usr/bin/env python3
"""
Refactor internal/ai sub-packages:
1. Fix import formatting (sed added imports without proper tab)
2. Prefix ai package types with ai. in all sub-package files
3. Rename shared helpers (truncate -> ai.Truncate, imgRefTypeLabel -> ai.ImgRefTypeLabel)
4. Remove locally-defined duplicate types from openai/openai.go (already done)
"""
import os
import re
import sys

BASE = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', 'internal', 'ai')
AI_IMPORT = '"github.com/inkframe/inkframe-backend/internal/ai"'

# All exported types/functions/constants from the ai package that need ai. prefix
# These are symbols defined in files remaining in internal/ai/ (provider.go, video_provider.go, etc.)
AI_SYMBOLS = [
    # --- Interfaces ---
    'AIProvider', 'VideoProvider', 'LipSyncProvider', 'MultimodalEmbedder',
    # --- Request/Response types (provider.go) ---
    'GenerateRequest', 'GenerateResponse', 'ChatMessage',
    'ImageGenerateRequest', 'ImageResponse', 'ControlNet',
    'AudioGenerateRequest', 'AudioResponse', 'TTSSubtitle',
    # --- Video types (video_provider.go) ---
    'VideoGenerateRequest', 'VideoTask', 'VideoTaskStatus',
    # --- LipSync types (lipsync_provider.go) ---
    'LipSyncRequest', 'LipSyncTask', 'LipSyncTaskStatus',
    # --- Multimodal embed types (provider.go) ---
    'MultimodalEmbedRequest', 'MultimodalEmbedResponse', 'MultimodalEmbedItem',
    'MultimodalSparseEmbeddingConfig', 'MultimodalMultiEmbeddingConfig',
    'SparseEmbedPoint',
    # --- Manager types (provider.go) ---
    'ModelManager', 'ImageProviderEntry',
    'GenerateRequestBuilder',
    'CostEstimator', 'UsageLogger', 'UsageLogEntry', 'UsageStats',
    'ModelHealthChecker', 'HealthStatus',
    'FallbackManager',
    # --- Engine traits (capabilities.go) ---
    'VideoEngineTraits', 'ImageEngineTraits',
    # --- Wrappers (provider.go, concurrent_provider.go, rate_limit_provider.go) ---
    'RetryProvider', 'CircuitBreaker', 'ConcurrentProvider', 'RateLimitProvider',
    # --- Shared OpenAI types (now in shared_helpers.go) ---
    'OpenAIChatResponse', 'OpenAIStreamChunk', 'OpenAIEmbedResponse',
    'DALLEResponse', 'SSEReader', 'SSEEvent',
    # --- Constants ---
    'DefaultProviderTimeout', 'ErrPrefixSensitiveContent',
    # --- Functions ---
    'NewModelManager', 'NewGenerateRequestBuilder', 'NewRetryProvider',
    'NewCircuitBreaker', 'NewConcurrentProvider', 'NewRateLimitProvider',
    'NewSSEReader', 'ResolveTimeout', 'FormatJSON',
    'RegisterVideoEngineTraits', 'RegisterImageEngineTraits',
    'VideoEngineTraitsFor', 'ImageEngineTraitsFor',
    'IsTimeoutError',
    # --- Shared helpers (renamed, in shared_helpers.go) ---
    'Truncate', 'ImgRefTypeLabel', 'RedactBase64Fields',
    # --- Provider name constants (defined in sub-packages but referenced from service layer) ---
    # These are LOCAL to each sub-package, so DON'T prefix them.
    # We handle them separately in the service layer update.
]

# Old unexported names -> new exported ai. qualified names
RENAMES = {
    'truncate': 'ai.Truncate',
    'imgRefTypeLabel': 'ai.ImgRefTypeLabel',
    'redactBase64Fields': 'ai.RedactBase64Fields',
}

SUBPACKAGES = [
    'kling', 'doubao', 'volcengine', 'dashscope', 'minimax',
    'openai', 'anthropic', 'deepseek', 'ollama', 'baidu',
    'tencent', 'elevenlabs', 'hunyuan',
]


def fix_imports(content, needs_ai_import):
    """Fix the ai import that was added by sed (missing tab) and ensure proper formatting."""
    # Remove the badly-formatted ai import line (no tab indentation)
    content = re.sub(r'\n"github\\.com/inkframe/inkframe-backend/internal/ai"\n', '\n', content)
    
    if needs_ai_import:
        # Check if ai import already exists (properly formatted)
        if AI_IMPORT not in content:
            # Add it at the beginning of the import block
            content = re.sub(
                r'(import \(\n)',
                r'\1\t' + AI_IMPORT + r'\n',
                content, count=1
            )
    return content


def scan_local_defs(lines):
    """Scan for locally-defined symbols (types, functions, constants, variables)."""
    local = set()
    for line in lines:
        stripped = line.strip()
        # func Name( or func (receiver) Name(
        m = re.match(r'^func\s+(?:\([^)]+\)\s+)?(\w+)', stripped)
        if m:
            local.add(m.group(1))
        # type Name struct/interface/...
        m = re.match(r'^type\s+(\w+)', stripped)
        if m:
            local.add(m.group(1))
        # const Name = or const ( Name = ... )
        m = re.match(r'^const\s+(\w+)', stripped)
        if m:
            local.add(m.group(1))
        # var Name = or var ( Name = ... )
        m = re.match(r'^var\s+(\w+)', stripped)
        if m:
            local.add(m.group(1))
    return local


def process_file(filepath, pkg_name):
    with open(filepath, 'r') as f:
        content = f.read()
    
    original = content
    
    # First, scan for local definitions
    local_defs = scan_local_defs(content.split('\n'))
    
    # Determine which AI symbols are actually used (and not locally defined)
    symbols_to_prefix = []
    for sym in AI_SYMBOLS:
        if sym not in local_defs:
            symbols_to_prefix.append(sym)
    
    # Check if file needs ai import
    needs_ai_import = False
    for sym in symbols_to_prefix:
        pattern = r'(?<![.\w])' + sym + r'\b'
        if re.search(pattern, content):
            needs_ai_import = True
            break
    for old in RENAMES:
        if old in local_defs:
            continue
        pattern = r'(?<![.\w])' + old + r'\b'
        if re.search(pattern, content):
            needs_ai_import = True
            break
    
    # Fix imports
    content = fix_imports(content, needs_ai_import)
    
    # Process line by line
    lines = content.split('\n')
    new_lines = []
    in_import_block = False
    
    for line in lines:
        stripped = line.strip()
        
        # Track import blocks
        if stripped.startswith('import ('):
            in_import_block = True
            new_lines.append(line)
            continue
        if in_import_block and stripped == ')':
            in_import_block = False
            new_lines.append(line)
            continue
        if in_import_block:
            new_lines.append(line)
            continue
        
        # Skip package declarations
        if stripped.startswith('package '):
            new_lines.append(line)
            continue
        
        # Skip single-line comments (but NOT inline comments after code)
        if stripped.startswith('//'):
            new_lines.append(line)
            continue
        
        # Process the line: prefix AI symbols
        for sym in symbols_to_prefix:
            pattern = r'(?<![.\w])' + sym + r'\b'
            line = re.sub(pattern, 'ai.' + sym, line)
        
        # Handle renames (truncate -> ai.Truncate, etc.)
        for old, new in RENAMES.items():
            if old in local_defs:
                continue
            pattern = r'(?<![.\w])' + old + r'\b'
            line = re.sub(pattern, new, line)
        
        new_lines.append(line)
    
    content = '\n'.join(new_lines)
    
    if content != original:
        with open(filepath, 'w') as f:
            f.write(content)
        print(f'Updated: {os.path.relpath(filepath, BASE)}')


def main():
    for pkg in SUBPACKAGES:
        pkg_dir = os.path.join(BASE, pkg)
        if not os.path.isdir(pkg_dir):
            print(f'Skipping {pkg} (directory not found)')
            continue
        for fname in sorted(os.listdir(pkg_dir)):
            if fname.endswith('.go'):
                process_file(os.path.join(pkg_dir, fname), pkg)
    print('Done!')


if __name__ == '__main__':
    main()
